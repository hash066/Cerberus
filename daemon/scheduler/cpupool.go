package scheduler

import (
	"encoding/hex"

	contract "github.com/hash066/cerberus/contract/go"
)

// ThreadsPerTask is how many CPU threads one compute dispatch occupies in the
// cluster pool. v0.1 uses 1; pipeline shards and WASM runs are accounted the same.
const ThreadsPerTask = 1

type cpuSlot struct {
	total uint32
	busy  uint32
}

// NodeCPU is one node's thread-pool occupancy as exposed on the status API.
type NodeCPU struct {
	PeerID string
	Total  uint32
	Free   uint32
	Busy   uint32
}

// ClusterCPU aggregates pooled CPU threads/cores across every known node.
type ClusterCPU struct {
	TotalCores uint32
	FreeCores  uint32
	BusyCores  uint32
	Nodes      []NodeCPU
}

// TotalCores returns logical CPU capacity from telemetry (P+E cores, min 1).
func TotalCores(t contract.NodeTelemetry) uint32 {
	n := t.Compute.PCores + t.Compute.ECores
	if n == 0 {
		return 1
	}
	return n
}

// totalCoresFor resolves a node's core count from telemetry, 0 when the node is
// unknown. It takes s.mu, so per the lock order (see Scheduler) it must be
// called BEFORE s.cpuMu is held — never from inside a cpuMu critical section.
func (s *Scheduler) totalCoresFor(id contract.PeerID) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tel, ok := s.nodes[id]; ok {
		return TotalCores(tel)
	}
	return 0
}

// FreeCPU returns unallocated threads on a node (0 when unknown or saturated).
func (s *Scheduler) FreeCPU(id contract.PeerID) uint32 {
	fallback := s.totalCoresFor(id) // mu BEFORE cpuMu — see lock order
	s.cpuMu.Lock()
	defer s.cpuMu.Unlock()
	return s.freeCPULocked(id, fallback)
}

// AcquireCPU reserves threads on a node. Returns false when the node is saturated.
func (s *Scheduler) AcquireCPU(id contract.PeerID, threads uint32) bool {
	if threads == 0 {
		return true
	}
	// Resolve the node's capacity from telemetry FIRST, under mu only. Doing this
	// lookup while holding cpuMu is what deadlocked against Place (which holds mu
	// and reaches for cpuMu through the cost model's FreeCPU call).
	fallback := s.totalCoresFor(id)

	s.cpuMu.Lock()
	defer s.cpuMu.Unlock()
	slot := s.cpuSlotLocked(id, fallback)
	if slot.total == 0 {
		// Node has no telemetry yet: assume a single thread rather than refuse.
		slot.total = 1
	}
	if slot.busy+threads > slot.total {
		return false
	}
	slot.busy += threads
	s.cpu[id] = slot
	return true
}

// ReleaseCPU returns threads to the pool after a dispatch completes. Releasing
// threads that were never acquired is a no-op: it must NOT materialize a slot,
// because a slot with total=0 reads as "zero free threads" forever after and
// would make the node permanently infeasible to the cost model and invisible to
// BestNode until its next telemetry tick re-synced it.
func (s *Scheduler) ReleaseCPU(id contract.PeerID, threads uint32) {
	if threads == 0 {
		return
	}
	s.cpuMu.Lock()
	defer s.cpuMu.Unlock()
	slot, ok := s.cpu[id]
	if !ok {
		return
	}
	if slot.busy <= threads {
		slot.busy = 0
	} else {
		slot.busy -= threads
	}
	s.cpu[id] = slot
}

// BestNode returns the node with the most free CPU threads, excluding any in
// exclude. ok is false when every candidate is saturated or unknown.
//
// This ranks on POOL OCCUPANCY ONLY — it is deliberately blind to host load,
// thermals, VRAM and power. Use BestNodeByCost for real placement; this stays
// for callers that specifically want "who has the most free slots".
func (s *Scheduler) BestNode(exclude map[contract.PeerID]bool) (contract.PeerID, uint32, bool) {
	// Snapshot ids AND their telemetry core counts under mu, then drop mu before
	// taking cpuMu (lock order: mu -> cpuMu, never the reverse).
	s.mu.Lock()
	nodeIDs := make([]contract.PeerID, 0, len(s.nodes))
	totals := make(map[contract.PeerID]uint32, len(s.nodes))
	for id, tel := range s.nodes {
		if exclude != nil && exclude[id] {
			continue
		}
		nodeIDs = append(nodeIDs, id)
		totals[id] = TotalCores(tel)
	}
	s.mu.Unlock()

	s.cpuMu.Lock()
	defer s.cpuMu.Unlock()

	var (
		best contract.PeerID
		free uint32
		ok   bool
	)
	for _, id := range nodeIDs {
		f := s.freeCPULocked(id, totals[id])
		if !ok || f > free {
			best, free, ok = id, f, true
		}
	}
	return best, free, ok && free > 0
}

// ClusterCPU returns pooled CPU capacity across the scheduler's node view.
func (s *Scheduler) ClusterCPU() ClusterCPU {
	s.mu.Lock()
	nodes := make(map[contract.PeerID]contract.NodeTelemetry, len(s.nodes))
	for id, tel := range s.nodes {
		nodes[id] = tel
	}
	s.mu.Unlock()

	s.cpuMu.Lock()
	defer s.cpuMu.Unlock()

	out := ClusterCPU{}
	for id, tel := range nodes {
		slot := s.cpuSlotLocked(id, TotalCores(tel))
		free := uint32(0)
		if slot.total > slot.busy {
			free = slot.total - slot.busy
		}
		out.TotalCores += slot.total
		out.BusyCores += slot.busy
		out.FreeCores += free
		out.Nodes = append(out.Nodes, NodeCPU{
			PeerID: hex.EncodeToString(id[:]),
			Total:  slot.total,
			Free:   free,
			Busy:   slot.busy,
		})
	}
	return out
}

func (s *Scheduler) syncCPUSlot(id contract.PeerID, tel contract.NodeTelemetry) {
	prev := s.cpu[id]
	total := TotalCores(tel)
	s.cpu[id] = cpuSlot{total: total, busy: prev.busy}
}

// cpuSlotLocked reads a node's slot. Caller must hold cpuMu. fallbackTotal is the
// node's telemetry core count (0 = unknown), resolved by the caller BEFORE taking
// cpuMu; it backfills total for a slot that has none. This function performs no
// locking of its own — it used to take s.mu here, which inverted the lock order.
func (s *Scheduler) cpuSlotLocked(id contract.PeerID, fallbackTotal uint32) cpuSlot {
	slot := s.cpu[id]
	if slot.total == 0 {
		slot.total = fallbackTotal
	}
	return slot
}

func (s *Scheduler) freeCPULocked(id contract.PeerID, fallbackTotal uint32) uint32 {
	slot := s.cpuSlotLocked(id, fallbackTotal)
	if slot.total <= slot.busy {
		return 0
	}
	return slot.total - slot.busy
}
