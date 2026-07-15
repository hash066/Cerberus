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

// FreeCPU returns unallocated threads on a node (0 when unknown or saturated).
func (s *Scheduler) FreeCPU(id contract.PeerID) uint32 {
	s.cpuMu.Lock()
	defer s.cpuMu.Unlock()
	return s.freeCPULocked(id)
}

// AcquireCPU reserves threads on a node. Returns false when the node is saturated.
func (s *Scheduler) AcquireCPU(id contract.PeerID, threads uint32) bool {
	if threads == 0 {
		return true
	}
	s.cpuMu.Lock()
	defer s.cpuMu.Unlock()
	slot := s.cpuSlotLocked(id)
	if slot.total == 0 {
		s.mu.Lock()
		if tel, ok := s.nodes[id]; ok {
			slot.total = TotalCores(tel)
		} else {
			slot.total = 1
		}
		s.mu.Unlock()
	}
	if slot.busy+threads > slot.total {
		return false
	}
	slot.busy += threads
	s.cpu[id] = slot
	return true
}

// ReleaseCPU returns threads to the pool after a dispatch completes.
func (s *Scheduler) ReleaseCPU(id contract.PeerID, threads uint32) {
	if threads == 0 {
		return
	}
	s.cpuMu.Lock()
	defer s.cpuMu.Unlock()
	slot := s.cpu[id]
	if slot.busy <= threads {
		slot.busy = 0
	} else {
		slot.busy -= threads
	}
	s.cpu[id] = slot
}

// BestNode returns the node with the most free CPU threads, excluding any in
// exclude. ok is false when every candidate is saturated or unknown.
func (s *Scheduler) BestNode(exclude map[contract.PeerID]bool) (contract.PeerID, uint32, bool) {
	s.mu.Lock()
	nodeIDs := make([]contract.PeerID, 0, len(s.nodes))
	for id := range s.nodes {
		if exclude != nil && exclude[id] {
			continue
		}
		nodeIDs = append(nodeIDs, id)
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
		f := s.freeCPULocked(id)
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
		slot := s.cpuSlotLocked(id)
		if slot.total == 0 {
			slot.total = TotalCores(tel)
		}
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

func (s *Scheduler) cpuSlotLocked(id contract.PeerID) cpuSlot {
	slot, ok := s.cpu[id]
	if !ok {
		s.mu.Lock()
		if tel, have := s.nodes[id]; have {
			slot.total = TotalCores(tel)
		}
		s.mu.Unlock()
	}
	return slot
}

func (s *Scheduler) freeCPULocked(id contract.PeerID) uint32 {
	slot := s.cpuSlotLocked(id)
	if slot.total <= slot.busy {
		return 0
	}
	return slot.total - slot.busy
}
