package chaos

import (
	"context"
	"sync"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/lifecycle"
	"github.com/hash066/cerberus/daemon/scheduler"
)

// recordingCoordinator mirrors cmd/cerberusd's lidDropCoordinator: it bridges the
// lifecycle monitor's SLEEP_IMMINENT choreography to scheduler.RerouteNode at the
// composition layer (neither lifecycle nor scheduler imports the other). It also
// records what happened so the test can assert the post-conditions of ARCHITECTURE
// §4.2 (checkpoint already done by the monitor before this is called; here we
// promote standbys + observe the handed-back caps).
type recordingCoordinator struct {
	sched *scheduler.Scheduler
	self  contract.PeerID

	mu        sync.Mutex
	promoted  []contract.Plan
	handBacks int
	handCaps  [][]byte
	resumed   int
}

func (c *recordingCoordinator) HandBackAndPromote(p lifecycle.SleepPrepare) error {
	plans := c.sched.RerouteNode(c.self)
	c.mu.Lock()
	c.handBacks++
	c.promoted = plans
	c.handCaps = p.HeldCaps
	c.mu.Unlock()
	return nil
}

func (c *recordingCoordinator) Resume(lifecycle.WakePrepare) error {
	c.mu.Lock()
	c.resumed++
	c.mu.Unlock()
	return nil
}

func (c *recordingCoordinator) snapshot() (handBacks int, promoted []contract.Plan, caps [][]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handBacks, append([]contract.Plan(nil), c.promoted...), append([][]byte(nil), c.handCaps...)
}

// TestLidDropCheckpointsAndPromotesStandby drives the full lid-drop sequence over
// the REAL lifecycle.Monitor + scheduler + CRDT engine, exactly as cmd/cerberusd
// composes them. The node "self" hosts a task as primary; a SLEEP_IMMINENT event
// arrives via the injectable source. We assert ARCHITECTURE §4.2 in order:
//
//  1. the monitor checkpointed the node's CRDT memory doc;
//  2. it pre-announced SLEEP_IMMINENT to subscribers;
//  3. the coordinator promoted a hot standby — the task moved OFF self;
//  4. the held capabilities were handed back to the coordinator.
func TestLidDropCheckpointsAndPromotesStandby(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newCluster(t, "self", "standby")
	self := c.node("self")

	// Place a task with "self" as the best node (AC + headroom), "standby" second.
	s := scheduler.New(nil)
	s.UpdateNode(telem("self", 8_000_000_000, 25, true))
	s.UpdateNode(telem("standby", 8_000_000_000, 15, true))
	task := contract.ComputeTask{TaskID: randBytes(8), Shard: contract.Shard{Kind: contract.ShardPipeline}}
	plan, err := s.Place(task)
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if plan.Placements[0].Node != peerID("self") {
		t.Fatalf("expected self primary, got %x", plan.Placements[0].Node)
	}

	// Wire the REAL monitor: CRDT engine for the checkpoint, channel source to
	// drive SLEEP_IMMINENT deterministically, coordinator → scheduler reroute.
	docID := randBytes(16)
	mon := lifecycle.NewMonitor(self.crdt, docID)
	src := lifecycle.NewChannelSource()
	mon.SetSource(src)
	coord := &recordingCoordinator{sched: s, self: peerID("self")}
	mon.SetCoordinator(coord)
	heldCaps := [][]byte{[]byte("cap-A"), []byte("cap-B")}
	mon.SetHeldCaps(heldCaps)

	// Observe the SLEEP_IMMINENT pre-announcement.
	var sawImminent atomicBool
	mon.OnTransition(func(ev lifecycle.LifecycleEvent) {
		if ev == lifecycle.SleepImminent {
			sawImminent.set()
		}
	})

	mon.Start(ctx)
	defer mon.Stop()

	// --- LID-DROP: the OS pre-announces imminent sleep. ---
	src.Push(lifecycle.PowerEvent{Kind: lifecycle.EvSleepImminent})

	// Wait for the choreography to complete (handback recorded).
	if !waitCond(2*time.Second, func() bool { h, _, _ := coord.snapshot(); return h > 0 }) {
		t.Fatal("lid-drop choreography did not fire (no HandBackAndPromote)")
	}

	// (1) Checkpoint: the monitor checkpointed the doc before going dark. The doc
	// now exists durably in the engine (Snapshot returns the persisted form).
	if _, err := self.crdt.Snapshot(docID); err != nil {
		t.Fatalf("checkpoint snapshot: %v", err)
	}

	// (2) SLEEP_IMMINENT pre-announced to subscribers.
	if !sawImminent.get() {
		t.Fatal("SLEEP_IMMINENT was not pre-announced to subscribers")
	}

	// (3) Standby promotion: the task placed on self moved off it.
	handBacks, promoted, gotCaps := coord.snapshot()
	if handBacks != 1 {
		t.Fatalf("expected exactly one hand-back, got %d", handBacks)
	}
	if len(promoted) != 1 {
		t.Fatalf("expected one task promoted off self, got %d", len(promoted))
	}
	if promoted[0].Placements[0].Node == peerID("self") {
		t.Fatal("task was not moved off the sleeping node")
	}
	if promoted[0].Placements[0].Node != peerID("standby") {
		t.Fatalf("expected promotion to standby, got %x", promoted[0].Placements[0].Node)
	}

	// (4) Held capabilities were handed back (no dangling authority on sleep).
	if len(gotCaps) != len(heldCaps) {
		t.Fatalf("expected %d held caps handed back, got %d", len(heldCaps), len(gotCaps))
	}

	// State machine reflects the imminent-sleep hint.
	if got := mon.State().Hint; got != contract.SleepImminent {
		t.Fatalf("monitor hint = %v, want SLEEP_IMMINENT", got)
	}
}

// TestLidCloseTriggersSleepPath asserts the laptop "lid slam" path: an
// EvLidClosed (not a graceful pre-announcement) is treated as imminent sleep and
// runs the same checkpoint + promote choreography (ARCHITECTURE §4.2, lid-drop).
func TestLidCloseTriggersSleepPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newCluster(t, "self", "standby")
	self := c.node("self")

	s := scheduler.New(nil)
	s.UpdateNode(telem("self", 8_000_000_000, 25, true))
	s.UpdateNode(telem("standby", 8_000_000_000, 15, true))
	task := contract.ComputeTask{TaskID: randBytes(8), Shard: contract.Shard{Kind: contract.ShardPipeline}}
	if _, err := s.Place(task); err != nil {
		t.Fatalf("place: %v", err)
	}

	mon := lifecycle.NewMonitor(self.crdt, randBytes(16))
	src := lifecycle.NewChannelSource()
	mon.SetSource(src)
	coord := &recordingCoordinator{sched: s, self: peerID("self")}
	mon.SetCoordinator(coord)

	var sawLid atomicBool
	mon.OnTransition(func(ev lifecycle.LifecycleEvent) {
		if ev == lifecycle.LidClosed {
			sawLid.set()
		}
	})

	mon.Start(ctx)
	defer mon.Stop()

	src.Push(lifecycle.PowerEvent{Kind: lifecycle.EvLidClosed})

	if !waitCond(2*time.Second, func() bool { h, _, _ := coord.snapshot(); return h > 0 }) {
		t.Fatal("lid-close did not trigger the sleep choreography")
	}
	if !sawLid.get() {
		t.Fatal("LID_CLOSED event was not emitted")
	}
	if _, promoted, _ := coord.snapshot(); len(promoted) != 1 || promoted[0].Placements[0].Node == peerID("self") {
		t.Fatalf("lid-close did not move the task off self: %+v", promoted)
	}
}

// --- small concurrency helpers --------------------------------------------

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (a *atomicBool) set()      { a.mu.Lock(); a.v = true; a.mu.Unlock() }
func (a *atomicBool) get() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

// waitCond polls cond until it is true or timeout elapses.
func waitCond(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
