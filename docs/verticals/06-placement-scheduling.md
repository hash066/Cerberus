# Vertical 06 — Placement & Scheduling Brain (NEW)

> **Workstream A — Core Compute & Cognition Plane.** Depends on: 08 (telemetry feed), 00 (caps), 01 (cluster events). Stub until integration: synthetic telemetry, transport. See [docs/workstreams.md](../workstreams.md).

> **Net-new vertical** surfaced during the debate: the directive said *what* to pool but never *who decides placement*. This is the single hardest problem in the system. Conforms to [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Purpose & Responsibilities
- Decide **what runs where, when**: map compute tasks/shards ([03](03-compute-orchestration.md)) onto nodes given live telemetry, capabilities, thermal/power state, and link quality.
- Re-route work on failure, throttle, or sleep — masterless and partition-tolerant.
- Optimize for **latency masking** (placement that keeps promise pipelines short) and **thermal fairness**.

## 2. Position in the System
- **Control plane.** Consumes telemetry (08/01), enforces capabilities (00), drives compute (03) and lifecycle (09).
- Masterless: each node runs a scheduler; epoch ownership for a contended resource is taken via a short Raft lease ([01](01-mesh-fabric-transport.md)), then released.

## 3. Detailed Architecture
```
   telemetry stream (08) ──► Cost Model ──► Placement Solver ──► Plan ──► Dispatch (03)
        compute/VRAM/             │              │                          │
        thermal/power/link        │              │            re-route on:  ▼
                                  │              │            - peer-down (01)
                                  │              │            - THERMAL_SHED (09)
                                  ▼              ▼            - SLEEP_IMMINENT (09)
                           capability-aware   constraint:   hot-standby promotion
                           filter (00)        fit ∧ cap ∧ thermal ∧ bw
```
- **Cost model:** scores a (task, node) pair from telemetry — VRAM fit, FLOPS, thermal headroom, link RTT/bandwidth to dependency shards, and battery/AC state. Pipeline placement prefers nodes adjacent on the activation path (minimize promise-pipeline hops).
- **Placement solver:** baseline is a **greedy bin-packer with constraints** (fit + capability + thermal + bandwidth); upgrade path is a periodic ILP/heuristic (e.g., simulated annealing) for multi-objective optimization. Hard constraints (capability, VRAM fit) are filters; soft objectives (latency, thermal balance) are the cost.
- **Re-routing:** on `peer-down`, `THERMAL_SHED`, or `SLEEP_IMMINENT` ([09](09-power-thermal-sleep.md)), the scheduler promotes a **hot standby** (work pre-duplicated to an adjacent node, Volume I §6.2) and re-mints the relevant capability to it.
- **Layer partitioning:** for model sharding, groups neural layers to fit each node's VRAM telemetry, exactly as Volume I described, but now capability-gated.

## 4. Data Structures / Wire Formats
- Input: `NodeTelemetry` ([schemas §2](../schemas/schemas.md)).
- Plan: `{task_id, placements: [{shard, node, caps}], standbys: [{shard, node}]}` published on `cerberus/<site>/sched/*`.
- Constraints derive from `Shard` + capability `quota` caveats.

## 5. Interfaces / APIs (Go)
```go
type Scheduler interface {
    Place(t ComputeTask) (Plan, error)
    OnEvent(e ClusterEvent)              // peer-down, thermal, sleep
    Reroute(taskID TaskID, lost NodeID) (Plan, error)
}
type CostModel interface { Score(t ComputeTask, n NodeID, tm NodeTelemetry) float64 }
```

## 6. Tech Stack
| Concern | Choice | Why |
|---|---|---|
| Language | Go | lives in the daemon; concurrency-friendly |
| Baseline solver | greedy constrained bin-packing | predictable, fast, good-enough placement |
| Advanced solver | ILP / simulated annealing (optional) | multi-objective optimization (frontier) |
| Telemetry source | Zenoh stream ([01](01-mesh-fabric-transport.md), [08](08-observability-tracing.md)) | live, low-overhead |
| Epoch lease | hashicorp/raft (scoped) | single-writer for contended placement |

## 7. Security Model
- The scheduler only places a task on a node where the **requesting principal holds (or can be delegated) the needed capability**; it cannot manufacture authority.
- Placement decisions are observable/traceable (08) for audit (Sealed).

## 8. Open Mesh vs Sealed
- **OpenMesh:** may place across orgs on the inter-site mesh, pricing via [05](05-eutxo-open-mesh-economy.md).
- **Sealed:** placement confined to org-attested nodes; optional affinity to TEE-capable hardware.

## 9. Failure Modes & Mitigations
| Failure | Mitigation |
|---|---|
| Telemetry stale across partition | treat stale nodes as unavailable; conservative placement |
| Thrashing (re-place loops) | hysteresis + min-dwell time per placement |
| No node fits a shard | further-split shard or queue with backpressure |
| Two schedulers place the same resource | Raft epoch lease arbitrates the contended resource |

## 10. Verdict
- **Baseline constrained scheduler: Shippable.**
- **Provably-optimal multi-objective placement: Frontier** (NP-hard; heuristics in practice).

Open questions: learned cost model vs hand-tuned; how aggressively to pre-duplicate hot standbys (cost vs resilience).
