// cpupool.go exposes pooled CPU compute — this node's and every mesh peer's —
// as capability-gated 9P devices under /cer/dev/cpu, the CPU counterpart to the
// VRAM/audio devices Compose already mounts. It is how "remote CPU" becomes a
// first-class, addressable resource in the namespace (vertical 04 + 06).
//
// WHAT IS REAL:
//   - The pool is driven by the SAME live telemetry the placement brain uses:
//     scheduler.NodeSnapshots() reflects the local node plus every peer whose
//     NodeTelemetry has arrived over the cap-gated mesh telemetry topic
//     (daemon/system/scheduler_loop.go). So a peer's CPU device appears exactly
//     when that peer is really present and reporting cores.
//   - Each device is registered with contract.KindCPU and a real per-node quota
//     derived from that node's reported cores (Flops from telemetry, a bounded
//     byte budget for streaming task I/O). Walk/Open are capability-checked by
//     the ninep.Server exactly like any other device — no ambient authority.
//   - Opening /cer/dev/cpu/<node>/ctl mints a capability-bound DATA-PLANE grant
//     through the same Granter Compose wired for every device, so a holder gets
//     a real QUIC transfer endpoint for the task's input/output bytes. Actual
//     WASM/CPU EXECUTION on the chosen peer rides the signed mesh compute RPC
//     (daemon/compute, `cerberus run --on <peer>`); the two compose the same way
//     a device ctl grant + the data plane do (ARCHITECTURE §4.1).
//
// WHAT IS A DOCUMENTED STUB (not faked): opening a remote CPU device's ctl does
// not itself auto-dispatch a workload — it allocates the transfer; the compute
// RPC is the execution trigger. Binding ctl-open directly to an auto-dispatch is
// a later convenience, exactly as the audio pool documents for its ctl→session.
package system

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/ninep"
)

const cpuPoolInterval = 5 * time.Second

// cpuIOQuotaBytes bounds the data-plane transfer a single CPU-device ctl open
// authorizes (task input/output streaming). Generous for shard-task payloads
// while still capping a runaway transfer.
const cpuIOQuotaBytes = 64 << 20 // 64 MiB

// cpuSnapshotSource is the slice of the scheduler the CPU pool needs: the live
// per-node telemetry view. *scheduler.Scheduler satisfies it. Declared here so
// the pool is unit-testable without a full scheduler and to keep the dependency
// one-directional.
type cpuSnapshotSource interface {
	NodeSnapshots() []contract.NodeTelemetry
}

// CPUPool registers /cer/dev/cpu/<node>/0 devices for the local node and every
// telemetry-known mesh peer, refreshing on a ticker so peers appear/disappear
// with the mesh. It publishes the merged list to the "cpu" source of the shared
// DeviceCatalog.
type CPUPool struct {
	ns      *ninep.Server
	src     cpuSnapshotSource
	self    contract.PeerID
	catalog *DeviceCatalog

	mu         sync.Mutex
	registered map[string]struct{} // device dirs this pool registered
}

// NewCPUPool builds a CPU device pool. catalog may be nil (namespace-only).
func NewCPUPool(ns *ninep.Server, src cpuSnapshotSource, self contract.PeerID, catalog *DeviceCatalog) *CPUPool {
	return &CPUPool{
		ns:         ns,
		src:        src,
		self:       self,
		catalog:    catalog,
		registered: map[string]struct{}{},
	}
}

// Run refreshes CPU devices until ctx is cancelled.
func (p *CPUPool) Run(ctx context.Context) {
	p.refresh()
	t := time.NewTicker(cpuPoolInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refresh()
		}
	}
}

// cpuDevicePath returns the namespace directory for a node's CPU device. The
// local node lives at /cer/dev/cpu/local/0; a peer at /cer/dev/cpu/<peer8>/0
// (mirroring the audio pool's short-hex peer segment).
func cpuDevicePath(node, self contract.PeerID) string {
	if node == self {
		return "/cer/dev/cpu/local/0"
	}
	return fmt.Sprintf("/cer/dev/cpu/%s/0", peerShortHex(node))
}

// cpuQuota derives a per-node CPU device quota from its telemetry: the reported
// available FLOPS and a bounded byte budget for streaming task I/O.
func cpuQuota(t contract.NodeTelemetry) contract.Quota {
	var flops uint64
	if t.Compute.Flops > 0 {
		flops = uint64(t.Compute.Flops)
	}
	return contract.Quota{Bytes: cpuIOQuotaBytes, Flops: flops}
}

// refresh reconciles the registered CPU devices with the current telemetry view.
func (p *CPUPool) refresh() {
	if p.ns == nil || p.src == nil {
		return
	}
	snaps := p.src.NodeSnapshots()

	newDirs := map[string]struct{}{}
	var entries []CatalogEntry
	for _, t := range snaps {
		dir := cpuDevicePath(t.PeerID, p.self)
		if _, dup := newDirs[dir]; dup {
			continue
		}
		q := cpuQuota(t)
		ref := contract.ResourceRef{Kind: contract.KindCPU, Path: dir, Quota: &q}
		p.ns.Register(dir, ref)
		newDirs[dir] = struct{}{}

		pooled := t.PeerID != p.self
		entry := CatalogEntry{
			Path:       dir,
			Kind:       string(contract.KindCPU),
			Pooled:     pooled,
			QuotaBytes: q.Bytes,
		}
		if pooled {
			entry.Peer = hex.EncodeToString(t.PeerID[:])
			entry.Name = fmt.Sprintf("CPU %d cores", cores(t))
		} else {
			entry.Name = fmt.Sprintf("Local CPU %d cores", cores(t))
		}
		entries = append(entries, entry)
	}

	// Drop devices for nodes that are no longer present.
	p.mu.Lock()
	for old := range p.registered {
		if _, ok := newDirs[old]; !ok {
			p.ns.Unregister(old)
		}
	}
	p.registered = newDirs
	p.mu.Unlock()

	if p.catalog != nil {
		p.catalog.setSource("cpu", entries)
	}
}

// cores returns a node's logical core count from telemetry (min 1).
func cores(t contract.NodeTelemetry) uint32 {
	n := t.Compute.PCores + t.Compute.ECores
	if n == 0 {
		return 1
	}
	return n
}
