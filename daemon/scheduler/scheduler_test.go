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

func TestPlaceGPUPicksNodeWithMostFreeVRAM(t *testing.T) {
	s := New(nil)
	s.UpdateNode(node(1, 2_000_000_000, false, true))  // 2GB, below need
	s.UpdateNode(node(2, 12_000_000_000, false, true)) // 12GB free -> best fit
	s.UpdateNode(node(3, 8_000_000_000, false, true))  // 8GB free -> standby
	s.UpdateNode(node(4, 24_000_000_000, true, true))  // throttling -> excluded

	plan, err := s.PlaceGPU([]byte{7}, 6_000_000_000) // need 6GB VRAM
	if err != nil {
		t.Fatalf("PlaceGPU: %v", err)
	}
	if plan.Placements[0].Node != (contract.PeerID{2}) {
		t.Fatalf("expected node 2 (most free VRAM meeting the need) primary, got %v", plan.Placements[0].Node[0])
	}
	if len(plan.Standbys) != 1 || plan.Standbys[0].Node != (contract.PeerID{3}) {
		t.Fatalf("expected node 3 standby (next-most free VRAM)")
	}
	// The recorded plan must be reroutable off the primary, proving PlaceGPU
	// participates in the same reroute machinery as Place.
	np, err := s.Reroute([]byte{7}, plan.Placements[0].Node)
	if err != nil {
		t.Fatalf("reroute gpu task: %v", err)
	}
	if np.Placements[0].Node != (contract.PeerID{3}) {
		t.Fatalf("expected standby node 3 promoted, got %v", np.Placements[0].Node[0])
	}
}

func TestPlaceGPUFailsWhenNoNodeHasEnoughVRAM(t *testing.T) {
	s := New(nil)
	s.UpdateNode(node(1, 1_000_000_000, false, true))
	s.UpdateNode(node(2, 3_000_000_000, false, true))
	if _, err := s.PlaceGPU([]byte{8}, 8_000_000_000); err == nil {
		t.Fatal("expected error when no node meets the VRAM need")
	}
}

// TestPlacePrefersLeastLoadedByAvailableFlops proves the cost model is
// load-aware: given two remote nodes identical in cores, VRAM, thermal, and AC
// power, the one advertising MORE available FLOPS (i.e. less loaded — the
// telemetry FLOPS is peak scaled by live host utilization, see
// daemon/system.localTelemetry) is chosen as primary for a CPU-bound task.
func TestPlacePrefersLeastLoadedByAvailableFlops(t *testing.T) {
	s := New(nil)
	busy := contract.NodeTelemetry{ // heavily loaded => low available FLOPS
		PeerID:  contract.PeerID{1},
		Compute: contract.Compute{PCores: 8, Flops: 0.1e12},
		Memory:  contract.Memory{VRAMFree: 8_000_000_000},
		Thermal: contract.Thermal{HeadroomC: 20},
		Power:   contract.Power{Src: contract.PowerAC},
	}
	idle := contract.NodeTelemetry{ // lightly loaded => high available FLOPS
		PeerID:  contract.PeerID{2},
		Compute: contract.Compute{PCores: 8, Flops: 0.9e12},
		Memory:  contract.Memory{VRAMFree: 8_000_000_000},
		Thermal: contract.Thermal{HeadroomC: 20},
		Power:   contract.Power{Src: contract.PowerAC},
	}
	s.UpdateNode(busy)
	s.UpdateNode(idle)

	plan, err := s.Place(task(70))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Placements[0].Node != (contract.PeerID{2}) {
		t.Fatalf("expected least-loaded node 2 (more available FLOPS) as primary, got %v", plan.Placements[0].Node[0])
	}
	if len(plan.Standbys) != 1 || plan.Standbys[0].Node != (contract.PeerID{1}) {
		t.Fatalf("expected loaded node 1 as standby")
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

// linkNode is `node` plus a per-peer link view, which is the input the RTT term
// of DefaultCostModel.Score consumes.
func linkNode(id byte, vramFree uint64, rttMs float64) contract.NodeTelemetry {
	n := node(id, vramFree, false, true)
	if rttMs >= 0 {
		n.Links = []contract.Link{{
			Peer:   contract.PeerID{99},
			Medium: contract.MediumWiFi,
			RTTms:  rttMs,
		}}
	}
	return n
}

// TestRTTPenaltyChangesPlacement proves the scheduler's link-RTT term is live
// code that changes a real decision — given that something actually populates
// NodeTelemetry.Links.
//
// READ THIS BEFORE BELIEVING THE RTT PENALTY DOES ANYTHING IN PRODUCTION: as of
// this commit NOTHING populates Links on the shipping path. daemon/system's
// localTelemetry (system.go:539) is the only Sample() source and it never sets
// the field, so n.Links is nil, `worst` is 0, and `score -= worst/100` subtracts
// zero on every node forever. daemon/system/links.go builds the real per-peer
// link view but localTelemetryWithLinks has no non-test caller — it is inert by
// its own admission. This test therefore pins the term's BEHAVIOUR, not a
// property the running daemon currently has.
func TestRTTPenaltyChangesPlacement(t *testing.T) {
	// Two nodes identical in every scored dimension except link RTT.
	s := New(nil)
	s.UpdateNode(linkNode(1, 8_000_000_000, 500)) // 500ms link
	s.UpdateNode(linkNode(2, 8_000_000_000, 1))   // 1ms link -> should win

	plan, err := s.Place(task(1))
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if len(plan.Placements) == 0 {
		t.Fatal("no placements")
	}
	if got := plan.Placements[0].Node; got != (contract.PeerID{2}) {
		t.Fatalf("placed on node %d, want node 2 (the 1ms link). The RTT term "+
			"(score -= worstRTT/100) did not change the decision", got[0])
	}

	// And with the links removed, the tie must resolve the other way (insertion
	// order / stable sort), proving it was the RTT that moved it and not chance.
	s2 := New(nil)
	s2.UpdateNode(linkNode(1, 8_000_000_000, -1)) // no links at all
	s2.UpdateNode(linkNode(2, 8_000_000_000, -1))
	plan2, err := s2.Place(task(1))
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if len(plan2.Placements) == 0 {
		t.Fatal("no placements")
	}
	if plan2.Placements[0].Node == (contract.PeerID{2}) {
		t.Log("NOTE: the no-link tie also resolves to node 2, so the check above is " +
			"weaker than intended: it would pass even if the RTT term did nothing")
	} else {
		t.Logf("confirmed: with no links the tie goes to node %d, so RTT is what moved it",
			plan2.Placements[0].Node[0])
	}
}

// TestRTTPenaltyIsTinyAgainstTheOtherTerms measures how much RTT it actually
// takes to overcome one unit of another signal, because the term's WEIGHT is as
// important as its existence and `score -= worstRTT/100` is very small.
//
// One free CPU thread is worth 5 points (freeCPU*5), so out-ranking a node with
// one extra free thread costs 500ms of RTT. On a LAN (~1ms) the penalty is 0.01
// points against a score that includes a flat HeadroomC of 30 — i.e. the RTT term
// cannot change any placement that is not otherwise an exact tie. This test pins
// that scale so the number is a measured fact rather than an impression.
func TestRTTPenaltyIsTinyAgainstTheOtherTerms(t *testing.T) {
	m := DefaultCostModel{}
	base, ok := m.Score(task(1), linkNode(1, 8_000_000_000, 0))
	if !ok {
		t.Fatal("base node infeasible")
	}
	for _, rtt := range []float64{1, 10, 100, 500} {
		got, _ := m.Score(task(1), linkNode(1, 8_000_000_000, rtt))
		t.Logf("RTT %6.0fms -> score %.4f (penalty %.4f of base %.4f = %.3f%%)",
			rtt, got, base-got, base, 100*(base-got)/base)
	}
	// A 1ms LAN link must cost exactly 0.01 points.
	got, _ := m.Score(task(1), linkNode(1, 8_000_000_000, 1))
	if d := base - got; d < 0.0099 || d > 0.0101 {
		t.Fatalf("1ms RTT penalty = %.6f, want 0.01 (score -= worstRTT/100)", d)
	}
}
