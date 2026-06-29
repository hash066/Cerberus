// Package scheduler is vertical 06 — the placement brain. It maps compute tasks
// onto nodes using live telemetry, a cost model, and capability feasibility,
// produces a primary placement plus a hot standby, and re-routes on node loss.
// Implements contract.Scheduler. Pure stdlib (no transport deps).
package scheduler

import (
	"encoding/hex"
	"sort"
	"sync"

	contract "github.com/hash066/cerberus/contract/go"
)

// CostModel scores a (task, node) pair. feasible=false means the node cannot run
// the task at all (hard constraint); score ranks feasible nodes (higher is better).
type CostModel interface {
	Score(t contract.ComputeTask, n contract.NodeTelemetry) (score float64, feasible bool)
}

// DefaultCostModel: a node is feasible if it is not thermally throttling and has
// free VRAM at least MinVRAM. Score rewards free VRAM and thermal headroom, with
// an AC-power bonus and a penalty for slow links.
type DefaultCostModel struct {
	MinVRAM uint64
}

func (m DefaultCostModel) Score(_ contract.ComputeTask, n contract.NodeTelemetry) (float64, bool) {
	if n.Thermal.Throttling || n.Memory.VRAMFree < m.MinVRAM {
		return 0, false
	}
	score := float64(n.Memory.VRAMFree)/1e9 + n.Thermal.HeadroomC
	if n.Power.Src == contract.PowerAC {
		score += 10
	}
	if n.Power.Hint == contract.SleepImminent {
		score -= 100 // avoid nodes about to sleep
	}
	// penalize the worst link RTT
	worst := 0.0
	for _, l := range n.Links {
		if l.RTTms > worst {
			worst = l.RTTms
		}
	}
	score -= worst / 100
	return score, true
}

// Scheduler holds the live node view and produces placements.
type Scheduler struct {
	mu         sync.Mutex
	nodes      map[contract.PeerID]contract.NodeTelemetry
	cost       CostModel
	placements map[string]contract.Plan
}

// New builds a scheduler with the given cost model (nil = DefaultCostModel).
func New(cost CostModel) *Scheduler {
	if cost == nil {
		cost = DefaultCostModel{}
	}
	return &Scheduler{
		nodes:      map[contract.PeerID]contract.NodeTelemetry{},
		cost:       cost,
		placements: map[string]contract.Plan{},
	}
}

// UpdateNode records the latest telemetry for a node.
func (s *Scheduler) UpdateNode(t contract.NodeTelemetry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[t.PeerID] = t
}

// rank returns feasible nodes (excluding `exclude`) best-first.
func (s *Scheduler) rank(t contract.ComputeTask, exclude map[contract.PeerID]bool) []contract.PeerID {
	type scored struct {
		id contract.PeerID
		sc float64
	}
	var cands []scored
	for id, tel := range s.nodes {
		if exclude[id] {
			continue
		}
		if sc, ok := s.cost.Score(t, tel); ok {
			cands = append(cands, scored{id, sc})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].sc > cands[j].sc })
	out := make([]contract.PeerID, len(cands))
	for i, c := range cands {
		out[i] = c.id
	}
	return out
}

// Place selects a primary node and a hot standby for the task's shard.
func (s *Scheduler) Place(t contract.ComputeTask) (contract.Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ranked := s.rank(t, nil)
	if len(ranked) == 0 {
		return contract.Plan{}, contract.Errf(contract.ErrThermalShed, "no feasible node")
	}
	plan := contract.Plan{
		TaskID:     t.TaskID,
		Placements: []contract.Placement{{Shard: t.Shard, Node: ranked[0], Caps: t.Caps}},
	}
	if len(ranked) > 1 {
		plan.Standbys = []contract.Placement{{Shard: t.Shard, Node: ranked[1], Caps: t.Caps}}
	}
	s.placements[hex.EncodeToString(t.TaskID)] = plan
	return plan, nil
}

// Reroute promotes the standby (or re-places excluding the lost node) when a node fails.
func (s *Scheduler) Reroute(taskID []byte, lost contract.PeerID) (contract.Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := hex.EncodeToString(taskID)
	prev, ok := s.placements[key]
	if !ok {
		return contract.Plan{}, contract.Errf(contract.ErrDenied, "unknown task")
	}
	// If the lost node was the primary and a standby exists, promote it.
	if len(prev.Placements) > 0 && prev.Placements[0].Node == lost && len(prev.Standbys) > 0 {
		newPlan := contract.Plan{
			TaskID:     taskID,
			Placements: []contract.Placement{prev.Standbys[0]},
		}
		// pick a fresh standby excluding both lost and the promoted node
		exclude := map[contract.PeerID]bool{lost: true, newPlan.Placements[0].Node: true}
		if ranked := s.rank(contract.ComputeTask{TaskID: taskID, Shard: prev.Placements[0].Shard}, exclude); len(ranked) > 0 {
			newPlan.Standbys = []contract.Placement{{Shard: prev.Placements[0].Shard, Node: ranked[0]}}
		}
		s.placements[key] = newPlan
		return newPlan, nil
	}
	// Otherwise re-place from scratch excluding the lost node.
	exclude := map[contract.PeerID]bool{lost: true}
	ranked := s.rank(contract.ComputeTask{TaskID: taskID, Shard: prevShard(prev)}, exclude)
	if len(ranked) == 0 {
		return contract.Plan{}, contract.Errf(contract.ErrThermalShed, "no feasible node after loss")
	}
	newPlan := contract.Plan{
		TaskID:     taskID,
		Placements: []contract.Placement{{Shard: prevShard(prev), Node: ranked[0]}},
	}
	if len(ranked) > 1 {
		newPlan.Standbys = []contract.Placement{{Shard: prevShard(prev), Node: ranked[1]}}
	}
	s.placements[key] = newPlan
	return newPlan, nil
}

func prevShard(p contract.Plan) contract.Shard {
	if len(p.Placements) > 0 {
		return p.Placements[0].Shard
	}
	return contract.Shard{}
}

// compile-time assertion that Scheduler satisfies the contract.
var _ contract.Scheduler = (*Scheduler)(nil)
