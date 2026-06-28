# Vertical 02 — Distributed State & Agent Memory (CRDTs)

> Owner archetype: **The Network/State Engineer** (with the AI Orchestrator). Conforms to [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Purpose & Responsibilities
- Hold **agent memory** (beliefs, working sets, KV state, logs) as **Conflict-free Replicated Data Types** so it merges deterministically across Wi-Fi partitions without a central database.
- Track causality with **vector clocks**.
- Crucially: **distinguish convergence from correctness.** CRDTs guarantee replicas converge to the *same* state; they do not guarantee that state is *right*. Contradictory agent inferences MUST be flagged to a human, never silently merged.

## 2. Position in the System
- **Control/state plane.** Ops travel over the Zenoh `cerberus/crdt/*` keys ([01](01-mesh-fabric-transport.md)).
- Upstream: OCap (writes need a `topic`/`memory` capability). Downstream: scheduler (06) and lifecycle (09) checkpoint via this engine; observability (08) traces merges.

## 3. Detailed Architecture
```
   agent (WASM)  ──memory.apply(delta)──►  CRDT Engine (Rust: automerge/yrs)
                                              │
        ┌─────────────────────────────────────┼───────────────────────────────┐
        ▼                                       ▼                               ▼
   Domain Reducers                        Vector Clock                    Delta Log
   kv | counter | set |                   per (doc, actor)                (append-only,
   log | agent.belief                                                      GC by causal stable cut)
        │
        ├─ agent.belief: contradiction detector ──► human-flag event (08) ; NO silent LWW
        └─ others: standard CRDT merge (LWW / PN / OR-set / RGA)

   partition heals ──► exchange missing deltas (by vector-clock diff) ──► merge ──► converge
```
- **Documents:** each agent (or shared blackboard) owns one or more CRDT documents addressed by `doc_id`.
- **Reducers by domain:** the `domain` field on each op ([schemas §3](../schemas/schemas.md)) selects merge semantics. `kv`→LWW register, `counter`→PN-counter, `set`→OR-set, `log`→RGA. `agent.belief` runs a **domain reducer** that detects mutually-contradictory assertions on the same subject and, instead of LWW, emits a `belief.conflict` event for human/over-agent adjudication.
- **Vector clocks:** every op carries a `VectorClock`; merge uses clock diffs to request only missing ops on reconnect (efficient anti-entropy).
- **GC:** delta log compacted at the causal stable cut (the frontier all live replicas have seen).

## 4. Data Structures / Wire Formats
- `CrdtOp` ([schemas §3](../schemas/schemas.md)): `{doc_id, actor, clock, domain, delta, cap, sig}`.
- `delta`: opaque Automerge/`yrs` binary change.
- `belief.conflict` event: `{doc_id, subject, candidates:[{actor, value, clock}], detected_at}` → observability (08) + tray (10).

## 5. Interfaces / APIs
WIT (guest): `resource memory { apply, snapshot }` ([schemas §7](../schemas/schemas.md)).
Rust core:
```rust
pub fn doc_open(doc_id: DocId, cap: CapId) -> Result<DocHandle, CapError>;
pub fn doc_apply(h: DocHandle, op: CrdtOp) -> Result<(), MergeError>;
pub fn doc_merge(h: DocHandle, remote: &[CrdtOp]) -> MergeReport; // includes conflicts
pub fn doc_snapshot(h: DocHandle) -> Vec<u8>;
pub fn doc_checkpoint(h: DocHandle) -> Vec<u8>;   // used by lifecycle (09) before sleep
```

## 6. Tech Stack
| Concern | Choice | Why |
|---|---|---|
| CRDT engine | **Automerge** (`automerge-rs`) and/or **`yrs`** (Yjs in Rust) | mature, battle-tested, binary deltas, JSON-like documents |
| Causality | vector clocks (per doc/actor) | efficient anti-entropy, partition reconciliation |
| Binding | Rust core ↔ Go via FFI | engine in Rust, orchestration in Go |
| Transport | Zenoh pub/sub ([01](01-mesh-fabric-transport.md)) | low-overhead delta dissemination |

## 7. Security Model
- Every op is signed by `actor` (Ed25519) and carries the `cap` that authorized the write; the engine rejects ops whose capability fails `cap_verify` ([00](00-ocap-security-kernel.md)).
- Reducers are deterministic and total — a malformed delta cannot diverge replicas (rejected before apply).
- Read access to a document requires a `memory`/`topic` capability scoped to that `doc_id`.

## 8. Open Mesh vs Sealed
- **OpenMesh:** documents may be shared cross-org when an explicit capability is delegated.
- **Sealed:** documents are org-scoped; `belief.conflict` events are written to the audit log with retention; optional PII redaction on snapshots.

## 9. Failure Modes & Mitigations
| Failure | Mitigation |
|---|---|
| Long partition, large divergence | vector-clock diff transfers only missing ops; bounded by delta-log GC |
| Contradictory beliefs merged wrongly | **`agent.belief` reducer flags, never auto-resolves**; human/over-agent decides |
| Malicious actor floods ops | capability + Gossipsub peer scoring ([01](01-mesh-fabric-transport.md)) rate-limit |
| Clock skew | logical vector clocks, not wall-clock, for causality (LWW tiebreak uses (clock, actor)) |

## 10. Verdict
- **CRDT merge & partition tolerance: Shippable** (Automerge/yrs are production-grade).
- **Semantic conflict policy for `agent.belief`: Frontier** — detection heuristics and the human-in-the-loop adjudication UX are open research. This is the honest-margins caveat: convergence is solved, *correctness* of contradictory agent memory is not.

Open questions: contradiction-detection model for free-form beliefs; whether to embed a small reasoning component to pre-triage conflicts before escalating.
