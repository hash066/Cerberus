package chaos

import (
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/scheduler"
)

// telem builds telemetry for a simulated node feasible to the DefaultCostModel
// (not throttling, ample VRAM). headroom/AC let us order which node is "best".
func telem(id string, vramFree uint64, headroom float64, ac bool) contract.NodeTelemetry {
	src := contract.PowerBattery
	if ac {
		src = contract.PowerAC
	}
	return contract.NodeTelemetry{
		PeerID:  peerID(id),
		Memory:  contract.Memory{VRAMFree: vramFree},
		Thermal: contract.Thermal{HeadroomC: headroom, Throttling: false},
		Power:   contract.Power{Src: src},
	}
}

// TestNodeLossMidTaskReroutesToStandby is ARCHITECTURE §4.2's node-loss recovery:
// a task is placed (primary + hot standby); the primary is LOST mid-task; the
// scheduler must reroute the task onto a node that is NOT the lost one — and,
// when a standby existed, promote exactly that standby. We assert the
// post-condition (the new primary), not merely that Reroute returned.
func TestNodeLossMidTaskReroutesToStandby(t *testing.T) {
	c := newCluster(t, "n1", "n2", "n3")
	_ = c // the cluster gives us stable PeerIDs via peerID(id)

	s := scheduler.New(nil)
	// n2 is the best (AC + most headroom) → primary; n1 second → standby; n3 worst.
	s.UpdateNode(telem("n1", 8_000_000_000, 15, true))
	s.UpdateNode(telem("n2", 8_000_000_000, 25, true))
	s.UpdateNode(telem("n3", 8_000_000_000, 5, false))

	task := contract.ComputeTask{TaskID: randBytes(8), Shard: contract.Shard{Kind: contract.ShardPipeline}}
	plan, err := s.Place(task)
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	primary := plan.Placements[0].Node
	if primary != peerID("n2") {
		t.Fatalf("expected n2 primary, got %x", primary)
	}
	if len(plan.Standbys) == 0 {
		t.Fatal("expected a hot standby to be planned")
	}
	standby := plan.Standbys[0].Node

	// --- NODE LOSS: the primary drops mid-task. ---
	newPlan, err := s.Reroute(task.TaskID, primary)
	if err != nil {
		t.Fatalf("reroute after node loss: %v", err)
	}
	got := newPlan.Placements[0].Node
	if got == primary {
		t.Fatalf("task still placed on the lost primary %x", primary)
	}
	if got != standby {
		t.Fatalf("expected standby %x promoted, got %x", standby, got)
	}
	// A fresh standby should be re-selected from the survivors (n3 is the only one
	// left after n2 lost and n1 promoted), so the task stays protected.
	if len(newPlan.Standbys) == 0 || newPlan.Standbys[0].Node == primary {
		t.Fatalf("expected a fresh survivor standby after reroute, got %+v", newPlan.Standbys)
	}
}

// TestNodeLossRerouteNodeMovesEveryTask covers the per-node failure path
// (scheduler.RerouteNode): a node hosting MULTIPLE tasks as primary drops, and
// every one of its tasks must move off it. This is the bulk reroute the lid-drop
// coordinator and the supervisor peer-down path both rely on.
func TestNodeLossRerouteNodeMovesEveryTask(t *testing.T) {
	c := newCluster(t, "n1", "n2")
	_ = c

	s := scheduler.New(nil)
	s.UpdateNode(telem("n1", 8_000_000_000, 15, true)) // standby
	s.UpdateNode(telem("n2", 8_000_000_000, 25, true)) // best → primary for both

	var tasks [][]byte
	for i := 0; i < 5; i++ {
		id := randBytes(8)
		if _, err := s.Place(contract.ComputeTask{TaskID: id, Shard: contract.Shard{Kind: contract.ShardPipeline}}); err != nil {
			t.Fatalf("place task %d: %v", i, err)
		}
		tasks = append(tasks, id)
	}

	lost := peerID("n2")
	plans := s.RerouteNode(lost)
	if len(plans) != len(tasks) {
		t.Fatalf("expected all %d tasks rerouted, got %d", len(tasks), len(plans))
	}
	for _, pl := range plans {
		if len(pl.Placements) == 0 {
			t.Fatalf("task %x has no placement after reroute", pl.TaskID)
		}
		if pl.Placements[0].Node == lost {
			t.Fatalf("task %x still on lost node after RerouteNode", pl.TaskID)
		}
	}
}

// TestNodeLossNoSurvivorFails asserts the honest failure mode: if the lost node
// was the only feasible one, reroute reports an error rather than silently
// placing nowhere or panicking.
func TestNodeLossNoSurvivorFails(t *testing.T) {
	s := scheduler.New(nil)
	s.UpdateNode(telem("solo", 8_000_000_000, 20, true))
	task := contract.ComputeTask{TaskID: randBytes(8), Shard: contract.Shard{Kind: contract.ShardPipeline}}
	if _, err := s.Place(task); err != nil {
		t.Fatalf("place: %v", err)
	}
	if _, err := s.Reroute(task.TaskID, peerID("solo")); err == nil {
		t.Fatal("expected an error rerouting with no surviving feasible node")
	}
}
