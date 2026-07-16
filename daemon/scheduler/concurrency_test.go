package scheduler

import (
	"sync"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
)

func feasible(id byte, cores uint32) contract.NodeTelemetry {
	return contract.NodeTelemetry{
		PeerID:  contract.PeerID{id},
		Compute: contract.Compute{PCores: cores, Flops: 1e12},
		Memory:  contract.Memory{VRAMFree: 8_000_000_000},
		Thermal: contract.Thermal{HeadroomC: 20},
		Power:   contract.Power{Src: contract.PowerAC},
	}
}

// TestPlaceRacingAcquireCPUDoesNotDeadlock is a regression test for an ABBA
// deadlock between the two locks this scheduler holds.
//
// Before the fix, the lock order was inconsistent:
//
//	Place -> rank -> DefaultCostModel.Score -> FreeCPU
//	    held mu, then reached for cpuMu
//	AcquireCPU(unknown peer) -> cpuSlotLocked
//	    held cpuMu, then reached for mu (to read the node's core count)
//
// Run concurrently, those two deadlock permanently — confirmed with a goroutine
// dump showing one goroutine blocked in cpuSlotLocked and another in
// DefaultCostModel.Score->FreeCPU, each holding the lock the other wanted. It
// had never fired in production only because Place had no production caller;
// wiring the cost model into the dispatch path (BestNodeByCost) makes Place and
// AcquireCPU genuinely concurrent, so this must stay green.
//
// The unknown peer matters: AcquireCPU only reached for mu when the peer had no
// cpu slot yet, i.e. a worker acquiring before its first telemetry tick — which
// is exactly what compute.WireWorker does on a fresh scheduler.
func TestPlaceRacingAcquireCPUDoesNotDeadlock(t *testing.T) {
	s := New(nil)
	s.UpdateNode(feasible(1, 8))

	var unknown contract.PeerID // never UpdateNode'd: forces the cpuMu -> mu path
	unknown[0] = 99

	const iterations = 50_000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			s.AcquireCPU(unknown, 1)
			s.ReleaseCPU(unknown, 1)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_, _ = s.Place(contract.ComputeTask{
				TaskID: []byte{byte(i), byte(i >> 8)},
				Shard:  contract.Shard{Kind: contract.ShardPipeline},
			})
		}
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock: Place raced against AcquireCPU and hung (lock-order inversion regressed)")
	}
}

// TestReleaseCPUWithoutAcquireDoesNotPoisonSlot pins a bug where ReleaseCPU on a
// peer with no slot MATERIALIZED one with total=0. Since a total=0 slot reports
// zero free threads, that peer then looked permanently saturated: infeasible to
// DefaultCostModel and skipped by BestNode, until its next telemetry tick
// happened to re-sync the slot. A release must never invent capacity state.
func TestReleaseCPUWithoutAcquireDoesNotPoisonSlot(t *testing.T) {
	s := New(nil)
	s.UpdateNode(feasible(1, 8))
	id := contract.PeerID{1}

	if free := s.FreeCPU(id); free != 8 {
		t.Fatalf("precondition: expected 8 free, got %d", free)
	}
	// Unbalanced release (an error path that released without acquiring).
	s.ReleaseCPU(id, ThreadsPerTask)

	if free := s.FreeCPU(id); free != 8 {
		t.Fatalf("stray ReleaseCPU poisoned the slot: expected 8 free, got %d", free)
	}
	if _, _, ok := s.BestNode(nil); !ok {
		t.Fatal("stray ReleaseCPU made the node invisible to BestNode")
	}
	if _, err := s.Place(task(1)); err != nil {
		t.Fatalf("stray ReleaseCPU made the node infeasible to Place: %v", err)
	}
}

// TestAcquireReleaseIsBalancedUnderConcurrency proves the pool has no drift: N
// concurrent acquire/release pairs must return the pool to exactly its starting
// occupancy, with no thread leaked and no negative-wrap freeing phantom threads.
func TestAcquireReleaseIsBalancedUnderConcurrency(t *testing.T) {
	s := New(nil)
	s.UpdateNode(feasible(1, 64))
	id := contract.PeerID{1}
	start := s.FreeCPU(id)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				if s.AcquireCPU(id, 1) {
					s.ReleaseCPU(id, 1)
				}
			}
		}()
	}
	wg.Wait()

	if got := s.FreeCPU(id); got != start {
		t.Fatalf("cpu pool drifted: started with %d free, ended with %d", start, got)
	}
	if c := s.ClusterCPU(); c.BusyCores != 0 {
		t.Fatalf("expected 0 busy cores after balanced acquire/release, got %d", c.BusyCores)
	}
}
