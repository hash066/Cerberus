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
	"github.com/hash066/cerberus/daemon/dataplane"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/ninep"
	"github.com/hash066/cerberus/daemon/scheduler"
	"github.com/hash066/cerberus/daemon/supervisor"
	"github.com/hash066/cerberus/daemon/telemetry"
)

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
	// NinePAddr is the 9P2000.L wire server's listen address (control plane).
	NinePAddr string
	// DataPlaneAddr is the QUIC data-plane receiver's listen address (bulk bytes).
	DataPlaneAddr string
	tree          *supervisor.Tree
	traceShutdown func(context.Context) error
}

// Compose wires every subsystem together against the frozen contract. The
// capability kernel authorizes the mesh, telemetry, and namespace; nothing acts
// on ambient authority.
func Compose(ctx context.Context, kernel contract.CapKernel, site string) (*System, error) {
	// Real intra-site mesh: libp2p + QUIC + mDNS, capability-gated.
	fab, err := mesh.New(ctx, mesh.Config{Site: site, Kernel: kernel, EnableMDNS: true})
	if err != nil {
		return nil, fmt.Errorf("mesh: %w", err)
	}

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
		_ = traceShutdown(context.Background())
		return nil, fmt.Errorf("telemetry: %w", err)
	}

	// Data plane: the capability-bound QUIC receiver that bulk bytes (tensors,
	// VRAM, audio, file content) actually flow over. It is physically separate
	// from the control plane (ARCHITECTURE.md §4.1) — the 9P namespace only mints
	// grants against it; payloads never traverse 9P.
	dp := dataplane.NewServer(kernel, time.Now().Unix())
	if err := dp.Listen("127.0.0.1:0"); err != nil {
		return nil, fmt.Errorf("dataplane listen: %w", err)
	}

	// The daemon's single data-plane Sink: a router that dispatches each authorized
	// inbound transfer by id. FS writes register a per-transfer handler that pipes
	// the bytes into dfs.Put; every other transfer (device/VRAM ctl grants) is
	// drained within its quota exactly as the prior nil sink did.
	router := newSinkRouter()

	// 9P capability namespace with a sample local VRAM device.
	ns := ninep.New(kernel)
	q := contract.Quota{Bytes: 2 * 1024 * 1024 * 1024}
	ns.Register("/cer/dev/vram/local/0",
		contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/local/0", Quota: &q})

	// /cer/fs — the distributed filesystem, backed by the dfs engine (Phase G2)
	// scattering shards into an in-memory ShardStore for v0.1. A write streams the
	// file bytes over the data plane into dfs.Put and records the returned Manifest
	// keyed by the path; a read resolves the path to its Manifest, runs dfs.Get,
	// and streams the reconstructed bytes back over the data plane. Bulk file bytes
	// ride the data plane, never 9P (ARCHITECTURE.md §3.5).
	//   NEXT STEPS (labelled): the path→Manifest map (MemMetaStore) is in-memory —
	//   a durable, transactional metadata store is next; and MemShardStore keeps
	//   shards on this node — peer scatter over the data plane is next.
	fsStore, err := newDFSFSStore(dp, router)
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
			Kind:     ninep.EndpointKind(ep.Kind),
			Endpoint: ep.Addr,
			StreamID: ep.TransferID,
			Quota:    ep.Quota,
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

	return &System{
		Kernel:        kernel,
		Fabric:        fab,
		Scheduler:     sched,
		Namespace:     ns,
		DataPlane:     dp,
		NinePAddr:     nineLn.Addr().String(),
		DataPlaneAddr: dp.Addr(),
		tree:          tree,
		traceShutdown: traceShutdown,
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
