// Package system composes the Cerberus control plane: the OCap kernel as the
// spine, the real libp2p/QUIC mesh fabric, telemetry, the placement scheduler,
// and the 9P capability namespace — all under one OTP-style supervision tree.
// This is the P0 integration seam that turns the merged lanes into one daemon.
package system

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/trace/noop"

	contract "github.com/hash066/cerberus/contract/go"
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
	tree      *supervisor.Tree
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

	var self contract.PeerID
	pub, err := telemetry.New(telemetry.Config{
		Fabric: fab,
		Cap:    topicCap,
		Site:   site,
		PeerID: self,
		Hz:     2,
		Sample: func() contract.NodeTelemetry { return localTelemetry(self) },
		Tracer: noop.NewTracerProvider().Tracer("cerberusd"),
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}

	// 9P capability namespace with a sample local VRAM device.
	ns := ninep.New(kernel)
	q := contract.Quota{Bytes: 2 * 1024 * 1024 * 1024}
	ns.Register("/cer/dev/vram/local/0",
		contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/local/0", Quota: &q})

	// Seed the scheduler with the local node so it can place work.
	sched := scheduler.New(nil)
	sched.UpdateNode(localTelemetry(self))

	tree := supervisor.New("cerberusd")
	tree.Supervise(supervisor.Permanent, runFunc(pub.Run))
	tree.Supervise(supervisor.Permanent, runFunc(func(c context.Context) error { return schedulerLoop(c, sched) }))

	return &System{Kernel: kernel, Fabric: fab, Scheduler: sched, Namespace: ns, tree: tree}, nil
}

// Serve runs the supervision tree until ctx is cancelled.
func (s *System) Serve(ctx context.Context) error { return s.tree.Serve(ctx) }

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
