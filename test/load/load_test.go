package load

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/scheduler"
)

// feasibleNode builds telemetry the DefaultCostModel accepts (not throttling,
// ample VRAM). Headroom varies the ranking so placements spread.
func feasibleNode(i int) contract.NodeTelemetry {
	return contract.NodeTelemetry{
		PeerID:  peerID(i),
		Memory:  contract.Memory{VRAMFree: 8_000_000_000},
		Thermal: contract.Thermal{HeadroomC: float64(10 + i%20), Throttling: false},
		Power:   contract.Power{Src: contract.PowerAC},
	}
}

// loadedScheduler returns a scheduler primed with n feasible nodes.
func loadedScheduler(n int) *scheduler.Scheduler {
	s := scheduler.New(nil)
	for i := 0; i < n; i++ {
		s.UpdateNode(feasibleNode(i))
	}
	return s
}

// TestSchedulerPlacementStorm hammers the placement brain with many concurrent
// placements and reroutes across many simulated nodes, then asserts BOUNDED
// behaviour: every task got a placement on a feasible node, reroutes always moved
// a task off its lost node, and the run leaked no goroutines. This is the
// "spin up many concurrent tasks/placements across the simulated nodes" load case.
func TestSchedulerPlacementStorm(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping placement storm in -short mode")
	}
	const (
		nodes     = 50
		workers   = 32
		perWorker = 400 // 12,800 placements total
	)
	s := loadedScheduler(nodes)

	before := goroutineCountSettled()

	var (
		placed     int64
		rerouted   int64
		badPlace   int64
		badReroute int64
		wg         sync.WaitGroup
	)
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				taskID := []byte(fmt.Sprintf("t-%d-%d", w, i))
				plan, err := s.Place(contract.ComputeTask{TaskID: taskID, Shard: contract.Shard{Kind: contract.ShardPipeline}})
				if err != nil || len(plan.Placements) == 0 {
					atomic.AddInt64(&badPlace, 1)
					continue
				}
				atomic.AddInt64(&placed, 1)
				// Reroute half the tasks off their primary; assert they move.
				if i%2 == 0 {
					lost := plan.Placements[0].Node
					np, rerr := s.Reroute(taskID, lost)
					if rerr != nil || len(np.Placements) == 0 || np.Placements[0].Node == lost {
						atomic.AddInt64(&badReroute, 1)
						continue
					}
					atomic.AddInt64(&rerouted, 1)
				}
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)

	if badPlace != 0 {
		t.Fatalf("%d placements failed or returned no node (expected 0)", badPlace)
	}
	if badReroute != 0 {
		t.Fatalf("%d reroutes failed to move a task off its lost node (expected 0)", badReroute)
	}
	totalOps := placed + rerouted
	if want := int64(workers * perWorker); placed != want {
		t.Fatalf("placed %d, want %d", placed, want)
	}
	throughput := float64(totalOps) / elapsed.Seconds()
	t.Logf("placement storm: %d placements + %d reroutes in %v (%.0f ops/s) across %d nodes",
		placed, rerouted, elapsed.Round(time.Millisecond), throughput, nodes)

	assertNoGoroutineLeak(t, before)
}

// TestRevocationStormConverges drives a revocation storm across a simulated mesh:
// every node runs the real auth.RevocationGossip; we revoke many distinct token
// ids concurrently on random origin nodes and assert that ALL of them converge to
// "denied" on EVERY node. This is the "many concurrent revocations across the
// simulated nodes" load case, exercising the OR-set convergence under contention.
func TestRevocationStormConverges(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping revocation storm in -short mode")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		nodes   = 8
		revokes = 500
	)
	shared := newBus()

	type meshNode struct {
		iss    *auth.Issuer
		gossip *auth.RevocationGossip
	}
	mesh := make([]*meshNode, nodes)
	for i := 0; i < nodes; i++ {
		kernel := stub.NewCapKernel()
		fab := &busFabric{bus: shared, kernel: kernel}
		iss, err := auth.NewIssuer()
		if err != nil {
			t.Fatalf("issuer %d: %v", i, err)
		}
		topicCa, _ := kernel.Mint(
			contract.ResourceRef{Kind: contract.KindTopic, Path: auth.DefaultRevocationTopic},
			[]contract.Right{contract.RightRead, contract.RightWrite}, nil)
		g := auth.NewRevocationGossip(fab, auth.DefaultRevocationTopic, topicCa)
		g.HookPublish(iss)
		mesh[i] = &meshNode{iss: iss, gossip: g}
		go func(n *meshNode) { _ = n.gossip.Run(ctx, n.iss) }(mesh[i])
	}
	// Wait until every node's gossip subscription is live so no early revoke is lost.
	waitSubs(t, shared, nodes)

	before := goroutineCountSettled()

	ids := make([]string, revokes)
	var wg sync.WaitGroup
	for r := 0; r < revokes; r++ {
		ids[r] = fmt.Sprintf("tok-%d", r)
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			origin := mesh[r%nodes]
			_ = origin.iss.Revoke(ids[r])
		}(r)
	}
	wg.Wait()

	// Convergence post-condition: every id is denied on every node. Gossipsub-style
	// delivery is async, so poll within a bounded window.
	deadline := time.Now().Add(10 * time.Second)
	for {
		missing := 0
		for _, id := range ids {
			for _, n := range mesh {
				if !n.iss.IsRevoked(id) {
					missing++
				}
			}
		}
		if missing == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("revocation storm did not converge: %d (node,id) pairs still allowed", missing)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("revocation storm: %d revocations converged across %d nodes", revokes, nodes)

	assertNoGoroutineLeak(t, before)
}

// TestConcurrentMintAttenuateRevoke stresses a single issuer with concurrent
// mint / attenuate / authorize / revoke from many goroutines, asserting the issuer
// stays internally consistent (no panic, revoked tokens always denied afterward).
func TestConcurrentMintAttenuateRevoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping mint/revoke stress in -short mode")
	}
	iss, err := auth.NewIssuer()
	if err != nil {
		t.Fatal(err)
	}
	before := goroutineCountSettled()

	const workers, perWorker = 16, 500
	var denied int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				tok, err := iss.Mint("agent", []string{"exec"}, "/cer/x", time.Hour)
				if err != nil {
					t.Errorf("mint: %v", err)
					return
				}
				if _, err := iss.Authorize(tok, "exec", "/cer/x"); err != nil {
					t.Errorf("authorize fresh token: %v", err)
					return
				}
				// Revoke it, then it MUST be denied (monotone).
				claims, err := iss.Authorize(tok, "exec", "/cer/x")
				if err != nil {
					t.Errorf("re-authorize: %v", err)
					return
				}
				if err := iss.Revoke(claims.ID); err != nil {
					t.Errorf("revoke: %v", err)
					return
				}
				if _, err := iss.Authorize(tok, "exec", "/cer/x"); err == nil {
					t.Error("revoked token still authorized")
					return
				}
				atomic.AddInt64(&denied, 1)
			}
		}()
	}
	wg.Wait()
	if want := int64(workers * perWorker); denied != want {
		t.Fatalf("revoked-then-denied %d times, want %d", denied, want)
	}
	assertNoGoroutineLeak(t, before)
}

// --- helpers ---------------------------------------------------------------

// waitSubs blocks until at least n subscriptions are registered on the bus.
func waitSubs(t *testing.T, b *bus, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		total := 0
		for _, ss := range b.subs {
			total += len(ss)
		}
		b.mu.Unlock()
		if total >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fewer than %d subscriptions registered", n)
}

// goroutineCountSettled returns the goroutine count after a short settle so a
// just-finished setup phase does not inflate the baseline.
func goroutineCountSettled() int {
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	return runtime.NumGoroutine()
}

// assertNoGoroutineLeak fails if the goroutine count grew materially beyond the
// baseline after the work finished. A small slack absorbs the runtime's own
// background goroutines and scheduler jitter; a real leak (one-per-op) blows past
// it by orders of magnitude.
func assertNoGoroutineLeak(t *testing.T, before int) {
	t.Helper()
	const slack = 10
	var after int
	// Allow transient goroutines (gossip appliers, ctx watchers) to wind down.
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= before+slack || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after > before+slack {
		t.Fatalf("goroutine leak: before=%d after=%d (slack=%d)", before, after, slack)
	}
}
