package scheduler

import (
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
)

// loaded builds telemetry for a node identical to every other node except for
// how busy its host CPU is. Flops is what localTelemetry advertises:
// peak x (1 - hostUtilization). Same cores, same VRAM, same thermal, same power
// — so load is the ONLY thing distinguishing these nodes.
func loaded(id byte, hostUtilization float64) contract.NodeTelemetry {
	const peak = 1e12 // matches daemon/system.basePeakFlops
	return contract.NodeTelemetry{
		PeerID:  contract.PeerID{id},
		Compute: contract.Compute{PCores: 8, Flops: peak * (1 - hostUtilization)},
		Memory:  contract.Memory{VRAMFree: 8_000_000_000},
		Thermal: contract.Thermal{HeadroomC: 20},
		Power:   contract.Power{Src: contract.PowerAC},
	}
}

// TestBestNodeIsBlindToHostLoad documents WHY the dispatch path could not have
// been telemetry-driven, and pins the limitation so nobody re-wires dispatch to
// BestNode believing it is load-aware.
//
// BestNode ranks on free POOL THREADS only. Two nodes with nothing dispatched to
// them both report 8/8 threads free regardless of host load, so a node pegged at
// 99% CPU and a fully idle node are indistinguishable to it. This is not a bug in
// BestNode — it answers "who has the most free slots", which is a real question —
// but it is the wrong question for placement.
func TestBestNodeIsBlindToHostLoad(t *testing.T) {
	s := New(nil)
	s.UpdateNode(loaded(1, 0.99)) // pegged
	s.UpdateNode(loaded(2, 0.01)) // idle
	pegged, idle := contract.PeerID{1}, contract.PeerID{2}

	if got := s.FreeCPU(pegged); got != 8 {
		t.Fatalf("pegged node should still report all pool threads free, got %d", got)
	}
	if got := s.FreeCPU(idle); got != 8 {
		t.Fatalf("idle node should report all pool threads free, got %d", got)
	}
	// Both tie at 8 free: BestNode cannot separate them, so whichever it returns
	// carries no load information. That is the whole point of this test.
	_, free, ok := s.BestNode(nil)
	if !ok || free != 8 {
		t.Fatalf("expected a tie at 8 free threads, got free=%d ok=%v", free, ok)
	}
}

// TestBestNodeByCostFollowsHostLoad is the claim: given nodes identical in every
// respect except live host load, the dispatch path's placement function picks the
// idle one — and follows the load when it moves.
func TestBestNodeByCostFollowsHostLoad(t *testing.T) {
	s := New(nil)
	pegged, idle := contract.PeerID{1}, contract.PeerID{2}

	s.UpdateNode(loaded(1, 0.99)) // node 1 pegged
	s.UpdateNode(loaded(2, 0.01)) // node 2 idle
	best, ok := s.BestNodeByCost(contract.ComputeTask{TaskID: []byte("t")}, nil)
	if !ok || best != idle {
		t.Fatalf("expected placement on the IDLE node 2, got %v ok=%v", best[0], ok)
	}

	// Flip the load: node 2 is now pegged and node 1 has gone idle. Placement
	// must follow — this is the "and it moves back" half of the claim.
	s.UpdateNode(loaded(1, 0.01))
	s.UpdateNode(loaded(2, 0.99))
	best, ok = s.BestNodeByCost(contract.ComputeTask{TaskID: []byte("t")}, nil)
	if !ok || best != pegged {
		t.Fatalf("placement did not follow the load back to node 1, got %v ok=%v", best[0], ok)
	}
}

// TestBestNodeByCostHonoursHardConstraints proves the dispatch path now refuses
// nodes it previously would have happily picked. BestNode applied NO feasibility
// filter beyond free threads, so dispatch could target a thermally throttling
// node or one about to sleep; the cost model rejects both.
func TestBestNodeByCostHonoursHardConstraints(t *testing.T) {
	t.Run("throttling node is not a placement target", func(t *testing.T) {
		s := New(nil)
		hot := loaded(1, 0.0) // totally idle, but throttling
		hot.Thermal.Throttling = true
		s.UpdateNode(hot)
		s.UpdateNode(loaded(2, 0.9)) // busy but healthy

		// BestNode would pick the throttling node (or tie with it) — it sees only
		// free threads, and the hot node has all 8.
		if free := s.FreeCPU(contract.PeerID{1}); free != 8 {
			t.Fatalf("precondition: throttling node reports %d free threads", free)
		}
		best, ok := s.BestNodeByCost(contract.ComputeTask{TaskID: []byte("t")}, nil)
		if !ok || best != (contract.PeerID{2}) {
			t.Fatalf("expected the busy-but-healthy node 2, got %v ok=%v", best[0], ok)
		}
	})

	t.Run("no feasible node reports failure rather than a bad pick", func(t *testing.T) {
		s := New(nil)
		hot := loaded(1, 0.0)
		hot.Thermal.Throttling = true
		s.UpdateNode(hot)
		if _, ok := s.BestNodeByCost(contract.ComputeTask{TaskID: []byte("t")}, nil); ok {
			t.Fatal("expected no feasible node when the only node is throttling")
		}
	})

	t.Run("saturated pool still excludes a node", func(t *testing.T) {
		s := New(nil)
		s.UpdateNode(loaded(1, 0.0)) // idle host...
		s.UpdateNode(loaded(2, 0.9))
		for i := 0; i < 8; i++ {
			if !s.AcquireCPU(contract.PeerID{1}, 1) { // ...but every thread taken
				t.Fatalf("acquire %d should succeed on an 8-thread node", i)
			}
		}
		best, ok := s.BestNodeByCost(contract.ComputeTask{TaskID: []byte("t")}, nil)
		if !ok || best != (contract.PeerID{2}) {
			t.Fatalf("expected node 2 once node 1's pool is saturated, got %v ok=%v", best[0], ok)
		}
	})
}

// TestBestNodeByCostIsDeterministicOnTies pins the map-iteration-order bug: two
// genuinely identical nodes must rank in a stable order, not a random one.
func TestBestNodeByCostIsDeterministicOnTies(t *testing.T) {
	first, ok := bestOfTwoIdenticalNodes(t)
	if !ok {
		t.Fatal("expected a feasible node")
	}
	for i := 0; i < 200; i++ {
		got, _ := bestOfTwoIdenticalNodes(t)
		if got != first {
			t.Fatalf("tie-break is non-deterministic: got %v then %v", first[0], got[0])
		}
	}
}

func bestOfTwoIdenticalNodes(t *testing.T) (contract.PeerID, bool) {
	t.Helper()
	s := New(nil)
	s.UpdateNode(loaded(1, 0.5))
	s.UpdateNode(loaded(2, 0.5))
	return s.BestNodeByCost(contract.ComputeTask{TaskID: []byte("t")}, nil)
}
