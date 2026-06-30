package scheduler

import (
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
)

func node(id byte, vramFree uint64, throttling bool, ac bool) contract.NodeTelemetry {
	src := contract.PowerBattery
	if ac {
		src = contract.PowerAC
	}
	return contract.NodeTelemetry{
		PeerID:  contract.PeerID{id},
		Memory:  contract.Memory{VRAMFree: vramFree},
		Thermal: contract.Thermal{HeadroomC: 20, Throttling: throttling},
		Power:   contract.Power{Src: src},
	}
}

func task(id byte) contract.ComputeTask {
	return contract.ComputeTask{TaskID: []byte{id}, Shard: contract.Shard{Kind: contract.ShardPipeline}}
}

func TestPlacePicksBestFeasibleNode(t *testing.T) {
	s := New(nil)
	s.UpdateNode(node(1, 4_000_000_000, false, false)) // 4GB, battery
	s.UpdateNode(node(2, 8_000_000_000, false, true))  // 8GB, AC -> best
	s.UpdateNode(node(3, 16_000_000_000, true, true))  // throttling -> infeasible

	plan, err := s.Place(task(10))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Placements[0].Node != (contract.PeerID{2}) {
		t.Fatalf("expected node 2 primary, got %v", plan.Placements[0].Node[0])
	}
	if len(plan.Standbys) != 1 || plan.Standbys[0].Node != (contract.PeerID{1}) {
		t.Fatalf("expected node 1 standby")
	}
}

func TestPlaceFailsWithNoFeasibleNode(t *testing.T) {
	s := New(nil)
	s.UpdateNode(node(1, 8_000_000_000, true, true)) // throttling only
	if _, err := s.Place(task(11)); err == nil {
		t.Fatal("expected error when no feasible node")
	}
}

func TestRereotePromotesStandby(t *testing.T) {
	s := New(nil)
	s.UpdateNode(node(1, 4_000_000_000, false, true))
	s.UpdateNode(node(2, 8_000_000_000, false, true)) // best -> primary
	plan, err := s.Place(task(12))
	if err != nil {
		t.Fatal(err)
	}
	primary := plan.Placements[0].Node
	standby := plan.Standbys[0].Node

	newPlan, err := s.Reroute([]byte{12}, primary)
	if err != nil {
		t.Fatal(err)
	}
	if newPlan.Placements[0].Node != standby {
		t.Fatalf("expected standby %v promoted, got %v", standby[0], newPlan.Placements[0].Node[0])
	}
}

func TestRerouteNodeMovesAllShardsOffLostNode(t *testing.T) {
	s := New(nil)
	s.UpdateNode(node(1, 4_000_000_000, false, true)) // standby
	s.UpdateNode(node(2, 8_000_000_000, false, true)) // best -> primary for both

	p1, err := s.Place(task(20))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Place(task(21)); err != nil {
		t.Fatal(err)
	}
	lost := p1.Placements[0].Node // node 2

	plans := s.RerouteNode(lost)
	if len(plans) != 2 {
		t.Fatalf("expected both tasks rerouted off the lost node, got %d", len(plans))
	}
	for _, pl := range plans {
		if pl.Placements[0].Node == lost {
			t.Fatalf("task %x still placed on lost node", pl.TaskID)
		}
	}
}

func TestPlacePipelineSpreadsShards(t *testing.T) {
	s := New(nil)
	s.UpdateNode(node(1, 8_000_000_000, false, true))
	s.UpdateNode(node(2, 8_000_000_000, false, true))
	s.UpdateNode(node(3, 8_000_000_000, false, true))

	shards := []contract.Shard{
		{Kind: contract.ShardPipeline, LayerLo: 0, LayerHi: 20},
		{Kind: contract.ShardPipeline, LayerLo: 21, LayerHi: 40},
		{Kind: contract.ShardPipeline, LayerLo: 41, LayerHi: 60},
	}
	plan, err := s.PlacePipeline([]byte{99}, shards)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Placements) != 3 {
		t.Fatalf("expected 3 shard placements, got %d", len(plan.Placements))
	}
	seen := map[contract.PeerID]bool{}
	for _, p := range plan.Placements {
		seen[p.Node] = true
	}
	if len(seen) != 3 {
		t.Fatalf("shards should spread across 3 distinct nodes, got %d", len(seen))
	}
}

func TestPlacePipelineNoNodes(t *testing.T) {
	s := New(nil)
	if _, err := s.PlacePipeline([]byte{1}, []contract.Shard{{}}); err == nil {
		t.Fatal("expected error with no feasible nodes")
	}
}

func TestMinVRAMConstraint(t *testing.T) {
	s := New(DefaultCostModel{MinVRAM: 6_000_000_000})
	s.UpdateNode(node(1, 4_000_000_000, false, true)) // below min -> infeasible
	s.UpdateNode(node(2, 8_000_000_000, false, true)) // ok
	plan, err := s.Place(task(13))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Placements[0].Node != (contract.PeerID{2}) {
		t.Fatal("expected only node 2 feasible")
	}
	if len(plan.Standbys) != 0 {
		t.Fatal("expected no standby (only one feasible node)")
	}
}
