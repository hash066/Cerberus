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
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/audio"
	"github.com/hash066/cerberus/daemon/audiolink"
	"github.com/hash066/cerberus/daemon/dataplane"
	"github.com/hash066/cerberus/daemon/dfs"
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

	// fsStore and localShards are kept for white-box tests (same package) that
	// need to inspect the /cer/fs Manifest a write produced or a node's own local
	// shard store directly — e.g. to prove a shard genuinely left this node for a
	// remote peer, rather than merely that a file round-trips (which would also
	// pass under an all-local placement bug). Not part of the public API.
	fsStore     *dfsFSStore
	localShards *dfs.MemShardStore
}

// DeviceRef is a namespace device Compose registered, for callers (the
// composition layer, tests) that need to know what got mounted without
// reaching into the ninep.Server's private device table.
type DeviceRef struct {
	Path string
	Kind contract.ResourceKind
}

// registerAudioDevices enumerates every real OS audio endpoint (Windows via
// WASAPI; an honest empty list on other platforms — see daemon/audio's
// EnumerateEndpoints doc) and mounts each one into the 9P namespace at
// /cer/dev/audio/<mic|speaker>/<index>, alongside the existing VRAM device.
// It never fails Compose: an enumeration error (or zero endpoints) just means
// zero audio devices are registered, exactly like a machine with no
// microphone plugged in — maturity honesty, not a hard dependency.
//
// DOCUMENTED STUB — cross-node audio SESSIONS are not composed here. This
// registers/discovers audio endpoints (so they list in `cerberus devices` and
// are capability-grantable), and the streaming transport exists as a tested
// library (daemon/audiolink over the QUIC data plane). A driveable SESSION now
// exists: `cerberus audio loopback` / DaemonRPC.AudioLoopback runs the full
// pipeline (control-plane grant, data-plane bytes, jitter-buffered
// reconstruction) over the REAL data plane on one node (audiolink.RunLoopback).
// What is NOT yet composed is a cross-NODE mic-to-speaker session: node B opening
// THIS node's registered speaker endpoint over the mesh and streaming its mic to
// it. That needs the device ctl-open to bind to a live speaker sink / mic source
// on each side plus a second node, and its end-to-end verification needs real
// audio hardware on two machines. Endpoints are registered + grantable and the
// transport is proven; the remaining gap is the cross-node device-open
// composition. Do not present remote mic/speaker sharing as working yet
// (maturity honesty).
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
		refs = append(refs, DeviceRef{Path: path, Kind: contract.KindAudio})
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

	var self contract.PeerID
	pub, err := telemetry.New(telemetry.Config{
		Fabric: fab,
		Cap:    topicCap,
		Site:   site,
		PeerID: self,
		Hz:     2,
		Sample: func() contract.NodeTelemetry { return localTelemetry(self) },
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

	// 9P capability namespace with a sample local VRAM device.
	//
	// GPU compute dispatch is now WIRED: daemon/gpu.Dispatch runs f32 kernels via
	// core/cabi's cerberus_gpu_submit (real wgpu device under `-tags ffi` +
	// `--features gpu` when an adapter is present, else a real pure-Go/Rust
	// software backend), exposed as `cerberus gpu <kernel>` / DaemonRPC.GpuDispatch
	// — and the result honestly reports which backend actually ran.
	//
	// This VRAM entry, however, is still only a capability-grantable namespace
	// resource: it lists in `cerberus devices` and opening its ctl mints a
	// data-plane grant, but the GPU dispatch path above does NOT yet place work
	// against this VRAM quota or route through the scheduler for remote placement —
	// so a peer cannot yet target THIS node's GPU by opening its device. Wiring
	// dispatch to the scheduler + VRAM-quota accounting is the remaining step;
	// none of it is a §8 Frontier item.
	ns := ninep.New(kernel)
	q := contract.Quota{Bytes: 2 * 1024 * 1024 * 1024}
	ns.Register("/cer/dev/vram/local/0",
		contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/local/0", Quota: &q})

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
	var meta MetaStore
	if db != nil {
		meta = NewBoltMetaStore(db)
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
	localShards := dfs.NewMemShardStore()
	fab.ServeShards(NewLocalShardServer(localShards), mesh.SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)
	scatterShards := NewRemoteScatterShardStore(localShards, fab, shardSigner, site)

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

	fsStore, err := newDFSFSStore(dp, router, scatterShards, meta)
	if err != nil {
		return nil, fmt.Errorf("dfs fs store: %w", err)
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
	ns.SetGranter(func(cap contract.CapHandle, _ contract.ResourceRef, transferID uint64, quota contract.Quota) (ninep.DataEndpoint, error) {
		ep := dp.RegisterGrant(transferID, cap, quota)
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
	sched.UpdateNode(localTelemetry(self))

	tree := supervisor.New("cerberusd")
	tree.Supervise(supervisor.Permanent, runFunc(pub.Run))
	tree.Supervise(supervisor.Permanent, runFunc(func(c context.Context) error { return schedulerLoop(c, sched) }))
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

	composeOK = true
	return &System{
		Kernel:        kernel,
		Fabric:        fab,
		Scheduler:     sched,
		Namespace:     ns,
		DataPlane:     dp,
		Site:          site,
		NinePAddr:     nineLn.Addr().String(),
		DataPlaneAddr: dp.Addr(),
		AudioDevices:  audioDevices,
		tree:          tree,
		traceShutdown: traceShutdown,
		fsStore:       fsStore,
		localShards:   localShards,
	}, nil
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

// schedulerLoop keeps the scheduler service alive and is where periodic
// re-placement/telemetry consumption is wired in.
func schedulerLoop(ctx context.Context, _ *scheduler.Scheduler) error {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func localTelemetry(self contract.PeerID) contract.NodeTelemetry {
	return contract.NodeTelemetry{
		PeerID:  self,
		Compute: contract.Compute{PCores: 8, Flops: 1e12},
		Memory:  contract.Memory{RAMTotal: 16_000_000_000, RAMFree: 8_000_000_000, VRAMTotal: 8_000_000_000, VRAMFree: 6_000_000_000},
		Thermal: contract.Thermal{HeadroomC: 30},
		Power:   contract.Power{Src: contract.PowerAC},
	}
}
