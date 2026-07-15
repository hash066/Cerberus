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

// DefaultCostModel: a node is feasible if it is not thermally throttling, has
// free VRAM at least MinVRAM, and has at least one free CPU thread in the pool.
// Score rewards free CPU (primary for WASM/thread-pool dispatch), free VRAM, and
// thermal headroom, with an AC-power bonus and a penalty for slow links.
type DefaultCostModel struct {
	MinVRAM uint64
	Pool    *Scheduler // optional; when set, saturated nodes are infeasible
}

func (m DefaultCostModel) Score(_ contract.ComputeTask, n contract.NodeTelemetry) (float64, bool) {
	if n.Thermal.Throttling || n.Memory.VRAMFree < m.MinVRAM {
		return 0, false
	}
	freeCPU := float64(TotalCores(n))
	if m.Pool != nil {
		freeCPU = float64(m.Pool.FreeCPU(n.PeerID))
		if freeCPU <= 0 {
			return 0, false
		}
	}
	// Compute.Flops is the node's currently-AVAILABLE FLOPS (peak scaled by live
	// host CPU utilization — see daemon/system.localTelemetry). Rewarding it makes
	// placement load-aware: between two nodes with equal free threads, VRAM, and
	// thermal headroom, the less-loaded one (more available FLOPS) wins, so a
	// CPU-bound task lands on the idlest node in the mesh. Normalized to TFLOPS so
	// it is a soft tie-breaker, not a term that dwarfs the free-thread signal.
	score := freeCPU*5 + n.Compute.Flops/1e12 + float64(n.Memory.VRAMFree)/1e9 + n.Thermal.HeadroomC
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
	cpuMu      sync.Mutex
	cpu        map[contract.PeerID]cpuSlot
	cost       CostModel
	placements map[string]contract.Plan
}

// New builds a scheduler with the given cost model (nil = DefaultCostModel wired
// to this scheduler's CPU pool).
func New(cost CostModel) *Scheduler {
	s := &Scheduler{
		nodes:      map[contract.PeerID]contract.NodeTelemetry{},
		cpu:        map[contract.PeerID]cpuSlot{},
		placements: map[string]contract.Plan{},
	}
	if cost == nil {
		cost = DefaultCostModel{Pool: s}
	} else if dcm, ok := cost.(DefaultCostModel); ok && dcm.Pool == nil {
		dcm.Pool = s
		cost = dcm
	}
	s.cost = cost
	return s
}

// UpdateNode records the latest telemetry for a node, preserving in-flight CPU
// occupancy for that peer.
func (s *Scheduler) UpdateNode(t contract.NodeTelemetry) {
	s.mu.Lock()
	s.nodes[t.PeerID] = t
	s.mu.Unlock()
	s.cpuMu.Lock()
	s.syncCPUSlot(t.PeerID, t)
	s.cpuMu.Unlock()
}

// NodeSnapshots returns the latest telemetry for every known node (local + peers).
func (s *Scheduler) NodeSnapshots() []contract.NodeTelemetry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]contract.NodeTelemetry, 0, len(s.nodes))
	for _, t := range s.nodes {
		out = append(out, t)
	}
	return out
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

// PlacePipeline spreads a multi-shard task (pipeline/tensor parallelism) across
// distinct feasible nodes, round-robin over the ranked candidates, assigning a
// hot standby per shard where capacity allows. This is the scale path: a model
// too big for one node is split and placed across the mesh.
func (s *Scheduler) PlacePipeline(taskID []byte, shards []contract.Shard) (contract.Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ranked := s.rank(contract.ComputeTask{TaskID: taskID}, nil)
	if len(ranked) == 0 {
		return contract.Plan{}, contract.Errf(contract.ErrThermalShed, "no feasible node")
	}
	plan := contract.Plan{TaskID: taskID}
	for i, sh := range shards {
		primary := ranked[i%len(ranked)]
		plan.Placements = append(plan.Placements, contract.Placement{Shard: sh, Node: primary})
		if len(ranked) > 1 {
			standby := ranked[(i+1)%len(ranked)]
			plan.Standbys = append(plan.Standbys, contract.Placement{Shard: sh, Node: standby})
		}
	}
	s.placements[hex.EncodeToString(taskID)] = plan
	return plan, nil
}

// PlaceGPU places a GPU/VRAM-bound task on the node that has the most free VRAM
// among those meeting minVRAM, reading live telemetry (NodeTelemetry.Memory.
// VRAMFree). It is the placement path for work that must run on a real GPU with
// enough headroom — e.g. a ComputeTask whose worker will open a peer's
// /cer/dev/gpu device and dispatch kernels over the data plane. A thermally
// throttling node is never chosen even if it reports free VRAM (the same hard
// constraint DefaultCostModel applies). The best-fit node is primary and the
// next-best a hot standby, exactly as Place does; ErrThermalShed is returned when
// no node has enough free VRAM.
//
// Unlike Place (which ranks through the configured CostModel), this ranks strictly
// by free VRAM so a GPU-bound task lands where the tensors will actually fit,
// independent of the CPU-oriented cost weighting. It records the plan under taskID
// so Reroute/RerouteNode apply to it too.
func (s *Scheduler) PlaceGPU(taskID []byte, minVRAM uint64) (contract.Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	type scored struct {
		id   contract.PeerID
		vram uint64
	}
	var cands []scored
	for id, tel := range s.nodes {
		if tel.Thermal.Throttling || tel.Memory.VRAMFree < minVRAM {
			continue
		}
		cands = append(cands, scored{id, tel.Memory.VRAMFree})
	}
	if len(cands) == 0 {
		return contract.Plan{}, contract.Errf(contract.ErrThermalShed, "no node with enough free VRAM")
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].vram > cands[j].vram })

	shard := contract.Shard{Kind: contract.ShardData}
	plan := contract.Plan{
		TaskID:     taskID,
		Placements: []contract.Placement{{Shard: shard, Node: cands[0].id}},
	}
	if len(cands) > 1 {
		plan.Standbys = []contract.Placement{{Shard: shard, Node: cands[1].id}}
	}
	s.placements[hex.EncodeToString(taskID)] = plan
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

// RerouteNode promotes standbys for every task currently placed (as primary) on
// the lost node — the scheduler side of the "lid-drop" choreography
// (ARCHITECTURE §4.2: when a node announces SLEEP_IMMINENT, its shards move to
// hot standbys before it goes dark). Returns the new plan for each affected task.
func (s *Scheduler) RerouteNode(lost contract.PeerID) []contract.Plan {
	s.mu.Lock()
	var keys []string
	for key, plan := range s.placements {
		for _, p := range plan.Placements {
			if p.Node == lost {
				keys = append(keys, key)
				break
			}
		}
	}
	s.mu.Unlock()

	var out []contract.Plan
	for _, key := range keys {
		taskID, err := hex.DecodeString(key)
		if err != nil {
			continue
		}
		if np, rerr := s.Reroute(taskID, lost); rerr == nil {
			out = append(out, np)
		}
	}
	return out
}

func prevShard(p contract.Plan) contract.Shard {
	if len(p.Placements) > 0 {
		return p.Placements[0].Shard
	}
	return contract.Shard{}
}

// compile-time assertion that Scheduler satisfies the contract.
var _ contract.Scheduler = (*Scheduler)(nil)
