// Package system composes the Cerberus control plane: the OCap kernel as the
// spine, the real libp2p/QUIC mesh fabric, telemetry, the placement scheduler,
// and the 9P capability namespace — all under one OTP-style supervision tree.
// This is the P0 integration seam that turns the merged lanes into one daemon.
package system

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/audio"
	"github.com/hash066/cerberus/daemon/audiolink"
	"github.com/hash066/cerberus/daemon/dataplane"
	"github.com/hash066/cerberus/daemon/dfs"
	"github.com/hash066/cerberus/daemon/gpu"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/ninep"
	"github.com/hash066/cerberus/daemon/scheduler"
	"github.com/hash066/cerberus/daemon/store"
	"github.com/hash066/cerberus/daemon/supervisor"
	"github.com/hash066/cerberus/daemon/telemetry"
)

// meshListenAddrs is the QUIC listen multiaddr for the mesh fabric.
//
// It defaults to LOOPBACK so unit tests (which Compose many fabrics in one
// process / one LAN) stay isolated and never auto-discover each other or a real
// daemon. cmd/cerberusd sets CERBERUS_MESH_LISTEN to /ip4/0.0.0.0/... so the
// SHIPPING daemon binds all interfaces and is reachable from other machines
// (LAN, or a Tailscale/WireGuard overlay) — the previous loopback-only default
// made cross-machine mesh impossible even though mDNS discovery ran.
func meshListenAddrs() []string {
	if v := os.Getenv("CERBERUS_MESH_LISTEN"); v != "" {
		return []string{v}
	}
	return []string{"/ip4/127.0.0.1/udp/0/quic-v1"}
}

// runFunc adapts a Serve-style function into a supervised service.
type runFunc func(context.Context) error

func (f runFunc) Serve(ctx context.Context) error { return f(ctx) }

// System is the composed control plane.
type System struct {
	Kernel    contract.CapKernel
	Fabric    contract.Fabric
	Scheduler *scheduler.Scheduler
	Namespace *ninep.Server
	DataPlane *dataplane.Server
	// Site is the intra-site domain the mesh fabric and its capability-scoped
	// services (shard placement, audio sessions) were composed with. The RPC layer
	// reads it to mint session capabilities against the right per-site resource
	// (e.g. mesh.AudioResource(Site)) so the serving peer's gate accepts them.
	Site string
	// NinePAddr is the 9P2000.L wire server's listen address (control plane).
	NinePAddr string
	// DataPlaneAddr is the QUIC data-plane receiver's listen address (bulk bytes).
	DataPlaneAddr string
	tree          *supervisor.Tree
	traceShutdown func(context.Context) error

	// AudioDevices is every audio endpoint Compose discovered and registered
	// into the namespace (see registerAudioDevices below). Populated
	// best-effort: on a platform/machine with no real backend or no endpoints
	// at all, this is simply empty — Compose never fails or fakes a device
	// for this. Exposed so the composition layer (cmd/cerberusd/main.go) can
	// mirror these into the same devices catalogue it already builds for VRAM
	// (the ninep.Server's own device table is private), which is in turn what
	// the new /api/v1/devices HTTP route lists.
	AudioDevices []DeviceRef

	// DeviceCatalog holds local + pooled remote devices for RPC/API listing.
	// Populated at Compose time and refreshed by AudioPool when the mesh has peers.
	DeviceCatalog *DeviceCatalog

	// Pool aggregates local + peer peripherals (storage scatter, GPU worker,
	// CPU-aware scheduler feed, audio registry) for the cluster resources API.
	Pool *PeripheralPool

	// fsStore and localShards are kept for white-box tests (same package) that
	// need to inspect the /cer/fs Manifest a write produced or a node's own local
	// shard store directly — e.g. to prove a shard genuinely left this node for a
	// remote peer, rather than merely that a file round-trips (which would also
	// pass under an all-local placement bug). Not part of the public API.
	fsStore *dfsFSStore
	// localShards is the INTERFACE, not the in-memory type: a composed daemon
	// with a durable store gets a DiskShardStore here (shards must be as durable
	// as the metadata that names them), while db == nil keeps the mem store.
	localShards dfs.ShardStore
	fsMeta      MetaStore // replicated metadata store backing /cer/fs
}

// DeviceRef is a namespace device Compose registered, for callers (the
// composition layer, tests) that need to know what got mounted without
// reaching into the ninep.Server's private device table.
type DeviceRef struct {
	Path string
	Kind contract.ResourceKind
	// Name is the platform-friendly device name when known (audio endpoints).
	Name string
}

// registerAudioDevices enumerates every real OS audio endpoint (Windows via
// WASAPI, Linux via the PulseAudio native protocol — both pure-Go and both
// hardware-verified; macOS/BSD return an honest empty list, see daemon/audio's
// EnumerateEndpoints doc) and mounts each one into the 9P namespace at
// /cer/dev/audio/<mic|speaker>/<index>, alongside the existing VRAM device.
// It never fails Compose: an enumeration error (or zero endpoints) just means
// zero audio devices are registered, exactly like a machine with no
// microphone plugged in — maturity honesty, not a hard dependency.
//
// Local audio endpoints are registered here; cross-node SESSIONS are composed in
// Compose via fab.ServeAudio (daemon/mesh/audio.go) and driven by `cerberus
// audio play/monitor --on <peer>`. Remote peer endpoints are pooled into the
// namespace by AudioPool (audio_pool.go). Opening a pooled remote device's ctl
// auto-starts the matching mesh audio session (audio_ctl.go) and returns a
// mesh-audio endpoint descriptor; local audio ctl opens still mint a dataplane grant.
func registerAudioDevices(ns *ninep.Server) []DeviceRef {
	endpoints, err := audio.EnumerateEndpoints()
	if err != nil {
		// Not fatal: no devices registered, same as an empty list.
		return nil
	}
	counts := map[audio.EndpointKind]int{}
	var refs []DeviceRef
	for _, ep := range endpoints {
		kindDir := "speaker"
		if ep.Kind == audio.EndpointMic {
			kindDir = "mic"
		}
		idx := counts[ep.Kind]
		counts[ep.Kind] = idx + 1
		path := fmt.Sprintf("/cer/dev/audio/%s/%d", kindDir, idx)
		ns.Register(path, contract.ResourceRef{Kind: contract.KindAudio, Path: path})
		refs = append(refs, DeviceRef{Path: path, Kind: contract.KindAudio, Name: ep.Name})
	}
	return refs
}

// Compose wires every subsystem together against the frozen contract. The
// capability kernel authorizes the mesh, telemetry, and namespace; nothing acts
// on ambient authority.
//
// db is the daemon's single embedded bbolt store (see daemon/store,
// cmd/cerberusd/main.go) used to durably persist the /cer/fs path→Manifest
// mapping (BoltMetaStore, metastore.go) across a restart. Passing nil falls back
// to an in-memory metadata store (entries do not survive a restart) — useful for
// a caller that has no on-disk store to offer (e.g. an ephemeral test harness);
// the live daemon always passes its real store.
func Compose(ctx context.Context, kernel contract.CapKernel, site string, db *store.Store) (*System, error) {
	// Real intra-site mesh: libp2p + QUIC + mDNS, capability-gated.
	fab, err := mesh.New(ctx, mesh.Config{Site: site, Kernel: kernel, EnableMDNS: true, ListenAddrs: meshListenAddrs()})
	if err != nil {
		return nil, fmt.Errorf("mesh: %w", err)
	}
	// composeOK guards the deferred teardown below: it stays false (so every
	// subsystem constructed so far is torn down) until the very last line of
	// Compose, right before the success return. Without this, an error on any
	// later step (mint, listen, dfs store init) returned nil/err while leaking
	// the already-constructed libp2p host / QUIC listener / tracer — harmless
	// once, but on a daemon that retries Compose after a transient failure this
	// leaks a socket and a goroutine per attempt.
	composeOK := false
	defer func() {
		if !composeOK {
			_ = fab.Close()
		}
	}()

	// A topic capability authorizes telemetry publication.
	topicCap, err := kernel.Mint(
		contract.ResourceRef{Kind: contract.KindTopic, Path: "cerberus/" + site + "/telemetry"},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)
	if err != nil {
		return nil, fmt.Errorf("mint topic cap: %w", err)
	}

	// Real OpenTelemetry tracing, opt-in via CERBERUS_TRACE so the default daemon
	// stays quiet but operators get exported spans when they want them.
	tracer, traceShutdown := telemetry.NewTracerProvider(os.Getenv("CERBERUS_TRACE") != "")
	defer func() {
		if !composeOK {
			_ = traceShutdown(context.Background())
		}
	}()

	pub, err := telemetry.New(telemetry.Config{
		Fabric: fab,
		Cap:    topicCap,
		Site:   site,
		PeerID: fab.PeerID(),
		Hz:     2,
		Sample: func() contract.NodeTelemetry { return localTelemetry(fab.PeerID()) },
		Tracer: tracer,
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}

	// Data plane: the capability-bound QUIC receiver that bulk bytes (tensors,
	// VRAM, audio, file content) actually flow over. It is physically separate
	// from the control plane (ARCHITECTURE.md §4.1) — the 9P namespace only mints
	// grants against it; payloads never traverse 9P.
	//
	// Security: the data plane is a raw QUIC transport that does NOT carry
	// libp2p's own handshake, so (per daemon/mesh/security.go's documented gap)
	// it cannot rely on libp2p to authenticate its peers the way the mesh fabric
	// does. Instead it binds its OWN TLS certificate to the SAME real Ed25519
	// identity the mesh fabric uses (fab.Identity(), whose public half is
	// fab.PeerID()) — not a throwaway per-listener key — so a dialer who already
	// knows this node's PeerID can pin the data-plane cert to it (see
	// daemon/dataplane/tls.go), closing the MITM gap the fresh-random-cert
	// version had.
	dp := dataplane.NewServer(kernel, time.Now().Unix(), fab.Identity())
	if err := dp.Listen("127.0.0.1:0"); err != nil {
		return nil, fmt.Errorf("dataplane listen: %w", err)
	}
	defer func() {
		if !composeOK {
			_ = dp.Close()
		}
	}()

	// The daemon's single data-plane Sink: a router that dispatches each authorized
	// inbound transfer by id. FS writes register a per-transfer handler that pipes
	// the bytes into dfs.Put; every other transfer (device/VRAM ctl grants) is
	// drained within its quota exactly as the prior nil sink did.
	router := newSinkRouter()

	// 9P capability namespace with a local VRAM device and a local GPU device.
	//
	// GPU compute dispatch is WIRED two ways, both running daemon/gpu.Dispatch
	// (real wgpu under `-tags ffi --features gpu` when an adapter is present, else a
	// real pure-Go/Rust software backend — the result always reports which one
	// actually ran):
	//   1. The mesh path (`cerberus gpu <kernel> --on <peer>` / DaemonRPC.GpuDispatch)
	//      — a point-to-point signed-capability RPC (gpu.WireWorker, wired in
	//      peripheral.go).
	//   2. The 9P DEVICE path (this block): /cer/dev/gpu/local/0 is a
	//      capability-gated namespace device. A peer that holds a capability walks
	//      it and opens its ctl, which mints a real data-plane grant; the peer then
	//      dials that QUIC data-plane session and dispatches an f32 kernel over it
	//      (gpu.Serve, wired as the data-plane Responder below). This is the
	//      "mount a peer's GPU as a device" model (vertical 04 §3): the control
	//      plane grants, the data plane carries the work — no kernel byte touches 9P.
	//
	// The VRAM device stays a one-way byte-transfer grant (its ctl mints a bulk
	// data-plane session, e.g. for staging tensors). HONEST REMAINING GAP: the
	// scheduler can now PLACE a GPU/VRAM-bound task on a node by free-VRAM
	// telemetry (scheduler.PlaceGPU), but the composed daemon does not yet
	// auto-route an opened device to the placement result — that step remains.
	// It does NOT debit a live VRAM quota as sessions run, and deliberately so:
	// free VRAM is MEASURED (daemon/gpu.VRAMSnapshot), which sees EVERY consumer
	// on the box, whereas a debit ledger would know only Cerberus's own sessions
	// and would advertise "nothing booked, 4 GiB free" while a game holds 3 GiB.
	// See docs/gpu.md. None of this is a §8 Frontier item (no fake zk-WASM / RDMA).
	ns := ninep.New(kernel)
	// The VRAM device's quota is this node's MEASURED VRAM, not a guess. When the
	// probe cannot see a GPU (no driver, unsupported OS — see daemon/gpu/vramprobe*.go)
	// Total() is 0 and the device is registered with a zero quota, which fails
	// closed: a grant against it can carry no bytes. That is the honest outcome —
	// the previous fixed 2 GiB was advertised regardless of whether the machine had
	// any VRAM at all.
	q := contract.Quota{Bytes: gpu.VRAMSnapshot().Total()}
	ns.Register("/cer/dev/vram/local/0",
		contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/local/0", Quota: &q})
	gpuQ := contract.Quota{Bytes: 16 * 1024 * 1024} // 16 MiB per-request ceiling for an f32 kernel session
	ns.Register("/cer/dev/gpu/local/0",
		contract.ResourceRef{Kind: contract.KindGPU, Path: "/cer/dev/gpu/local/0", Quota: &gpuQ})

	// Local CPU compute device. Opening its ctl mints a capability-bound
	// data-plane grant (like VRAM); remote peers' CPU devices are pooled into
	// /cer/dev/cpu/<peer8>/0 by CPUPool below, driven by the same live telemetry
	// the placement brain uses. This is the namespace face of remote CPU sharing
	// (vertical 04 + 06). Registered up front so /cer/dev/cpu/local/0 is openable
	// the instant the namespace serves, independent of the pool's first tick.
	cpuQ := contract.Quota{Bytes: cpuIOQuotaBytes, Flops: uint64(basePeakFlops)}
	ns.Register("/cer/dev/cpu/local/0",
		contract.ResourceRef{Kind: contract.KindCPU, Path: "/cer/dev/cpu/local/0", Quota: &cpuQ})

	// Real audio devices (Windows: every active WASAPI mic/speaker endpoint;
	// other platforms: none yet — see daemon/audio.EnumerateEndpoints). Each
	// discovered endpoint is mounted at /cer/dev/audio/<mic|speaker>/<index>
	// next to the VRAM device above, so it is capability-gated exactly the
	// same way and shows up automatically in anything that lists the
	// namespace (the CLI's `cerberus devices`, and the tray's /api/v1/devices).
	audioDevices := registerAudioDevices(ns)

	// /cer/fs — the distributed filesystem, backed by the dfs engine (Phase G2). A
	// write streams the file bytes over the data plane into dfs.Put and durably
	// records the returned Manifest keyed by the path; a read resolves the path to
	// its Manifest, runs dfs.Get, and streams the reconstructed bytes back over the
	// data plane. Bulk file bytes ride the data plane, never 9P (ARCHITECTURE.md
	// §3.5).
	//
	// Metadata durability: the path→Manifest map is BoltMetaStore (bbolt-backed,
	// metastore.go) when db is non-nil, so it survives a daemon restart; nil falls
	// back to the in-memory MemMetaStore.
	var localMeta MetaStore
	if db != nil {
		localMeta = NewBoltMetaStore(db)
	} else {
		localMeta = NewMemMetaStore()
	}

	// Shard placement: this node's own local shard store, wrapped so every Nth
	// shard scatters onto a currently-known mesh peer over the real mesh RPC
	// (daemon/mesh's ServeShards/RequestPutShard/RequestGetShard) instead of
	// staying on this node — see shardstore.go for the v1 round-robin policy.
	// ServeShards lets OTHER peers place shards HERE using the same mechanism,
	// GATED by a signed capability (daemon/mesh/shard.go): the caller must
	// present a valid RightWrite/RightRead capability over
	// mesh.MeshShardResource(site) before any local store access happens. The
	// signer is keyed off this node's own mesh identity (fab.Identity()) so the
	// issuer PeerID a minted cap names always equals the PeerID the QUIC/TLS
	// handshake authenticates for this node's outbound streams — the trust model
	// shard.go documents (self-issued, stream-bound, since there is no separate
	// peer-key-exchange/discovery protocol in this daemon to hang a per-peer
	// grant off of).
	shardSigner, err := NewShardCapSigner(fab.Identity())
	if err != nil {
		return nil, fmt.Errorf("shard cap signer: %w", err)
	}
	// This node's own shard bytes. DURABILITY MUST MATCH THE METADATA STORE: when
	// db is non-nil the path->Manifest map above is bbolt-backed and survives a
	// restart, so the SHARDS must too. Pairing durable metadata with an in-memory
	// shard store (which is what this was) meant /cer/fs came back after a restart
	// still listing every file and unable to read any of them — `fs get` failed
	// with "too few shards given: have 0 valid shards, need 4". A filesystem that
	// confidently lists data it has lost is worse than one that admits it stored
	// nothing.
	//
	// Shards live beside cerberus.db so both halves of a node's /cer/fs state sit
	// in one directory and share its lifetime. db == nil (tests) keeps the
	// in-memory store, which is then correct rather than lossy: nothing else in
	// that configuration is durable either.
	var localShards dfs.ShardStore
	if db != nil && db.Path() != "" {
		shardDir := filepath.Join(filepath.Dir(db.Path()), "shards")
		disk, derr := dfs.NewDiskShardStore(shardDir)
		if derr != nil {
			return nil, fmt.Errorf("open durable shard store: %w", derr)
		}
		localShards = disk
	} else {
		localShards = dfs.NewMemShardStore()
	}
	fab.ServeShards(NewLocalShardServer(localShards), mesh.SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)
	fab.ServeMeta(NewLocalMetaServer(localMeta), mesh.SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)
	scatterShards := NewRemoteScatterShardStore(localShards, fab, shardSigner, site)
	meta := NewReplicatedMetaStore(localMeta, fab, shardSigner, site)

	// Cross-node real-time audio (mic/speaker sharing): serve the responder side of
	// a capability-gated mesh audio session (daemon/mesh/audio.go). A remote peer
	// that presents a valid signed capability over mesh.AudioResource(site) can
	// PLAY into THIS node's live speaker (RightWrite) or MONITOR THIS node's live
	// microphone (RightRead); the gate is verified — self-issued, stream-bound via
	// mesh.SelfIssuerResolver, exactly like ServeShards above — before any audio
	// device is opened. The live backends are daemon/audio's WASAPI Source/Sink
	// (real on Windows; documented stubs elsewhere), so on a machine with no real
	// backend the session opens, authorizes, then fails loudly on device open
	// rather than faking audio. The requester side (`cerberus audio play/monitor
	// --on <peer>`) is driven from the daemon RPC via fab.OpenAudioSession.
	fab.ServeAudio(
		audiolink.NewLiveMeshAudioServer(audiolink.DefaultFormat, audio.ReceiverConfig{}),
		mesh.SelfIssuerResolver,
		func() int64 { return time.Now().Unix() },
		nil,
	)

	// Cross-node audio device enumeration: peers can query THIS node's WASAPI
	// endpoints (cap-gated) so their AudioPool can register them as pooled
	// peripherals. Uses the same self-issued stream-bound trust model as shards.
	fab.ServeAudioDevices(
		osAudioLister{},
		mesh.SelfIssuerResolver,
		func() int64 { return time.Now().Unix() },
		nil,
	)

	// Device catalog + audio pool: merge local endpoints with remote peers' mics
	// and speakers into one listing for RPC/tray. The pool runs under supervision.
	catalog := &DeviceCatalog{}
	localCatalog := []CatalogEntry{
		{Path: "/cer/dev/vram/local/0", Kind: string(contract.KindVRAM), QuotaBytes: q.Bytes},
		{Path: "/cer/dev/gpu/local/0", Kind: string(contract.KindGPU), QuotaBytes: gpuQ.Bytes},
	}
	for _, ad := range audioDevices {
		localCatalog = append(localCatalog, CatalogEntry{
			Path: ad.Path, Kind: string(ad.Kind), Name: ad.Name,
		})
	}
	catalog.set(localCatalog)
	audioPool := NewAudioPool(ns, fab, shardSigner, site, localCatalog, catalog)

	fsStore, err := newDFSFSStore(dp, router, scatterShards, meta)
	if err != nil {
		return nil, fmt.Errorf("dfs fs store: %w", err)
	}

	// Pipeline worker: signed mesh compute for split-MLP shards + activation grants
	// on the data plane (vertical 03 layer-split demo path).
	if err := registerPipelineServices(dp, fab, kernel, router, site); err != nil {
		return nil, fmt.Errorf("pipeline: %w", err)
	}

	// The /cer/fs subtree is served by the namespace's own WalkFS/OpenFSWrite/
	// OpenFSRead methods (not the device Register path), so we install the backend
	// rather than registering a device resource. Wiring the backend also makes
	// /cer/fs walkable and read/writable; leaving it unwired keeps devices working
	// with /cer/fs reporting PARTITIONED.
	ns.SetFSStore(fsStore)

	// The cross-cut wiring (HANDOFF Phase F "next"): opening a device `.../ctl`
	// allocates a real transfer on the data plane and hands back the endpoint the
	// holder dials — the control plane mints the grant, the data plane moves the
	// bytes. The capability that opened ctl is the same one the data-plane server
	// will verify when the holder connects, so the grant is end-to-end authorized.
	//
	// A KindGPU device grants a REQUEST/RESPONSE session (a dataplane.Responder
	// running gpu.Serve): the holder streams an f32 kernel over the granted session
	// and the node runs it on its GPU backend and returns the result. Every other
	// device (VRAM/bulk) grants the one-way byte transfer as before. Pooled remote
	// audio devices auto-start a mesh audio session (audio_ctl.go) and return a
	// mesh-audio endpoint instead. All are authorized by the capability the 9P
	// open checked.
	audioBinder := NewAudioCtlBinder(fab, shardSigner, site)
	ns.SetGranter(func(cap contract.CapHandle, ref contract.ResourceRef, transferID uint64, quota contract.Quota) (ninep.DataEndpoint, error) {
		if ep, bound, err := audioBinder.TryBind(ref.Path, transferID, quota); bound || err != nil {
			return ep, err
		}
		var ep dataplane.Endpoint
		if ref.Kind == contract.KindGPU {
			ep = dp.RegisterResponder(transferID, cap, quota, func(_ uint64, req []byte) ([]byte, error) {
				return gpu.Serve(req)
			})
		} else {
			ep = dp.RegisterGrant(transferID, cap, quota)
		}
		return ninep.DataEndpoint{
			Kind:         ninep.EndpointKind(ep.Kind),
			Endpoint:     ep.Addr,
			StreamID:     ep.TransferID,
			Quota:        ep.Quota,
			ServerPeerID: ep.ServerPeerID, // the daemon's own real identity; the holder pins its dial to it.
		}, nil
	})

	// A connection capability so the 9P wire server can serve the local namespace
	// over 9P2000.L. Per-connection capability negotiation is the seam documented
	// in daemon/ninep/wire.go; here the daemon serves its own local devices.
	devCap, err := kernel.Mint(
		contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/local/0"},
		[]contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	if err != nil {
		return nil, fmt.Errorf("mint device cap: %w", err)
	}
	nineLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("9p listen: %w", err)
	}
	defer func() {
		if !composeOK {
			_ = nineLn.Close()
		}
	}()
	wire := ninep.NewWireServer(ns)

	// Seed the scheduler with the local node so it can place work.
	sched := scheduler.New(nil)
	localSample := func() contract.NodeTelemetry { return localTelemetry(fab.PeerID()) }
	sched.UpdateNode(localSample())

	// CPU device pool: exposes this node's and every telemetry-known peer's CPU
	// as capability-gated /cer/dev/cpu/<node>/0 devices, refreshed from the live
	// scheduler node view. refresh() once now so the local CPU device and catalog
	// entry are present at Compose return; the supervised Run keeps peers current.
	cpuPool := NewCPUPool(ns, sched, fab.PeerID(), catalog)
	cpuPool.refresh()

	tree := supervisor.New("cerberusd")
	tree.Supervise(supervisor.Permanent, runFunc(pub.Run))
	tree.Supervise(supervisor.Permanent, runFunc(func(c context.Context) error {
		return schedulerLoop(c, schedulerLoopConfig{
			Sched:    sched,
			Fabric:   fab,
			Site:     site,
			TopicCap: topicCap,
			Sample:   localSample,
		})
	}))
	// Serve the data-plane receiver. The router sink dispatches each authorized
	// transfer: an FS write streams into dfs.Put, everything else drains within its
	// quota (the prior nil-sink behaviour for device grants).
	tree.Supervise(supervisor.Permanent, runFunc(func(c context.Context) error {
		return dp.Serve(c, router.route)
	}))
	// Serve the 9P2000.L control-plane namespace over the wire, closing the
	// listener on shutdown so Serve unblocks.
	tree.Supervise(supervisor.Permanent, runFunc(func(c context.Context) error {
		go func() { <-c.Done(); _ = nineLn.Close() }()
		if err := wire.Serve(nineLn, devCap); err != nil && c.Err() == nil {
			return err
		}
		return nil
	}))
	tree.Supervise(supervisor.Permanent, runFunc(func(c context.Context) error {
		audioPool.Run(c)
		return c.Err()
	}))
	tree.Supervise(supervisor.Permanent, runFunc(func(c context.Context) error {
		cpuPool.Run(c)
		return c.Err()
	}))

	composeOK = true
	sys := &System{
		Kernel:        kernel,
		Fabric:        fab,
		Scheduler:     sched,
		Namespace:     ns,
		DataPlane:     dp,
		Site:          site,
		NinePAddr:     nineLn.Addr().String(),
		DataPlaneAddr: dp.Addr(),
		AudioDevices:  audioDevices,
		DeviceCatalog: catalog,
		tree:          tree,
		traceShutdown: traceShutdown,
		fsStore:       fsStore,
		localShards:   localShards,
		fsMeta:        meta,
	}
	sys.Pool = wirePeripherals(sys, nil)
	return sys, nil
}

// Serve runs the supervision tree until ctx is cancelled, then flushes tracing.
func (s *System) Serve(ctx context.Context) error {
	err := s.tree.Serve(ctx)
	if s.traceShutdown != nil {
		// Flush exported spans on the way down; use a fresh context since ctx is
		// already cancelled by the time Serve returns.
		flushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.traceShutdown(flushCtx)
	}
	return err
}

// basePeakFlops is this node's nominal peak compute used as the reference the
// live host CPU utilization is applied against, so a busy box advertises fewer
// available FLOPS to the placement brain (see localTelemetry).
const basePeakFlops = 1e12

// localTelemetry samples this node's live resource picture for the placement
// brain and the telemetry publisher. CPU cores are real (runtime.NumCPU) and
// FLOPS are LOAD-ADJUSTED: available = peak × (1 − hostUtilization), sampled
// from the OS (daemon/system/cpuload*.go). A heavily loaded node therefore
// advertises less available compute, so the scheduler's cost model (which now
// rewards available FLOPS) keeps CPU-bound work off a saturated node and prefers
// an idle remote peer — the telemetry-driven placement of vertical 06.
func localTelemetry(self contract.PeerID) contract.NodeTelemetry {
	cores := uint32(runtime.NumCPU())
	if cores == 0 {
		cores = 1
	}
	load := hostCPULoad.Sample() // [0,1]; 0 on first read / unsupported OS
	availFlops := basePeakFlops * (1 - load)
	if availFlops < 0 {
		availFlops = 0
	}
	// VRAM is MEASURED (daemon/gpu/vramprobe*.go: nvidia-smi, amdgpu sysfs).
	// A node that cannot probe reports zero rather than a plausible number:
	// scheduler.PlaceGPU ranks by free VRAM, so an invented figure silently
	// misplaces work — the prior hardcoded 8GB/6GB literal was ~2x this dev
	// box's real 4GB card and would have OOM'd it. Zero is honest and simply
	// loses the ranking; a lie loses the job.
	//
	// RAM is still a literal — that is daemon/system/telemetry work in flight
	// on another lane; do not read these two numbers as equally trustworthy.
	vram := gpu.VRAMSnapshot()
	return contract.NodeTelemetry{
		PeerID:  self,
		Compute: contract.Compute{PCores: cores, Flops: availFlops},
		Memory: contract.Memory{
			RAMTotal:  16_000_000_000,
			RAMFree:   8_000_000_000,
			VRAMTotal: vram.Total(),
			VRAMFree:  vram.Free(),
		},
		Thermal: contract.Thermal{HeadroomC: 30},
		Power:   contract.Power{Src: contract.PowerAC},
	}
}
