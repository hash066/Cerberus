# Cerberus — Implementation Workstreams

> How the build is split for parallel development. Three workstreams own disjoint clubs of verticals, each builds and tests **standalone** against the frozen schema contract, and they converge through a fixed integration ladder. Conforms to [ARCHITECTURE.md](../ARCHITECTURE.md).

---

## 0. The rule that makes parallelism possible: the Frozen Contract

The integration contract is **frozen**: [ARCHITECTURE.md §3](../ARCHITECTURE.md) + [docs/schemas/schemas.md](schemas/schemas.md). It defines every cross-boundary type and interface:

- capability token (CBOR/CDDL), `cap_verify`/`is_revoked` semantics
- node telemetry (proto3), CRDT op envelope + vector clocks
- compute task + promise, 9P namespace + op→capability table
- settlement tx + zk/optimistic proof (CDDL), WIT worlds, IPC gRPC, error codes

**Every workstream codes against these interfaces. Anything a workstream *consumes* from another is replaced by a mock/stub that honors the schema, until integration.** A change to the contract is a cross-team event, not a local edit — propose, review, version-bump, then all three adopt.

```
                         ┌──────────────────────────────┐
                         │   FROZEN CONTRACT (schemas)   │
                         │   ARCHITECTURE.md §3 +         │
                         │   docs/schemas/schemas.md     │
                         └───────────────┬──────────────┘
            everyone codes to this contract; stubs fill the gaps
        ┌────────────────────┬───────────┴───────────┬────────────────────┐
        ▼                    ▼                       ▼
 ┌──────────────┐    ┌──────────────────┐    ┌──────────────────────┐
 │ WORKSTREAM A │    │   WORKSTREAM B   │    │     WORKSTREAM C      │
 │ Core Compute │    │  Connectivity &  │    │ Economy, Lifecycle &  │
 │ & Cognition  │    │   Trust Fabric   │    │       Surface         │
 └──────────────┘    └──────────────────┘    └──────────────────────┘
```

---

## 1. Workstream A — Core Compute & Cognition Plane

**Charter:** the substrate that thinks, remembers, computes, and grants. The capability mechanism, distributed memory, portable execution, peripheral access, and placement — the tightly-interlocking heart of the system.

| Vertical | Doc | Lang |
|---|---|---|
| 00 OCap Security Kernel (spine) | [00](verticals/00-ocap-security-kernel.md) | Rust |
| 02 Distributed State & CRDTs | [02](verticals/02-distributed-state-crdts.md) | Rust ↔ Go FFI |
| 03 Compute Orchestration (WASM, sharding, promise pipelining) | [03](verticals/03-compute-orchestration.md) | Rust |
| 04 9P Peripheral Virtualization | [04](verticals/04-9p-peripheral-virt.md) | Go |
| 06 Placement & Scheduling Brain | [06](verticals/06-placement-scheduling.md) | Go |

**Owns in the contract:** capability schema + `cap_verify`/`cap_mint`/`cap_attenuate`; the WIT host (`caps` interface); CRDT op envelope semantics; compute task + promise; 9P namespace + op→cap table.

**Standalone build target:** `cerberus-core` (Rust crates) + the 9P/scheduler Go daemon pieces, runnable as a **single-process multi-node simulation**.

**Standalone test plan:** mint a capability → attenuate it to 2 GiB → run a toy WASM component that calls a granted `gpu` handle → shard a tiny model across simulated in-process nodes with promise pipelining → write & merge a CRDT memory doc across a simulated partition (assert convergence + `belief.conflict` flagging) → schedule placement from synthetic telemetry.

**Consumes (stub until integration):**
| Need | Stub | Real source |
|---|---|---|
| live telemetry | synthetic telemetry generator | B / 08 |
| transport for deltas, activations, CapTP | in-process loopback | B / 01 |
| identity / revocation verifier | always-valid local verifier | B / 07 |

---

## 2. Workstream B — Connectivity & Trust Fabric

**Charter:** how nodes discover each other, prove who they are, and are observed. The wire, the admission gate, and the lens.

| Vertical | Doc | Lang |
|---|---|---|
| 01 Mesh Fabric & Transport (Zenoh + libp2p / QUIC / supervision) | [01](verticals/01-mesh-fabric-transport.md) | Go |
| 07 Identity & Capability Lifecycle | [07](verticals/07-identity-cap-lifecycle.md) | Rust |
| 08 Observability & Tracing | [08](verticals/08-observability-tracing.md) | Go |

**Owns in the contract:** Zenoh key space + topic capability gating; telemetry proto3 publication cadence; attestation envelope; `sys/revocations` OR-set doc + `is_revoked` semantics; OTel span format + trace-context propagation in CapTP frames.

**Standalone build target:** a mesh node binary that discovers peers, admits them, and emits/collects telemetry + traces — **no compute required**.

**Standalone test plan:** boot 2–3 real OS processes → assert Zenoh mDNS + libp2p DCUtR discovery → relay through a third node across a simulated Wi-Fi blind spot → run attested admission (accept valid quote, reject forged) → exercise OTP supervision (kill a child, assert restart strategy + backoff) → publish telemetry, subscribe, render a trace tree from stub workloads.

**Consumes (stub until integration):**
| Need | Stub | Real source |
|---|---|---|
| capability mechanism (`cap_verify` on topics) | stub cap kernel (accepts well-formed caps) | A / 00 |
| CRDT engine for the revocation OR-set | local CRDT lib instance | A / 02 |

> **Seam note:** 07 (capability *lifecycle*) is intentionally tightly coupled to 00 (capability *mechanism*, in A). The seam is the `cap_verify`/`is_revoked` API + the `sys/revocations` OR-set — both already in the frozen contract — so each side builds independently and meets at integration.

---

## 3. Workstream C — Economy, Lifecycle & Surface

**Charter:** the optional economy, the node's physical edge behavior, and the human/external surface. The parts a deployment can run *without* (Sealed has no economy; a server has no lid; headless has no tray) — which makes them cleanly separable.

| Vertical | Doc | Lang |
|---|---|---|
| 05 eUTXO & Open Mesh Economy (optional / profile-gated) | [05](verticals/05-eutxo-open-mesh-economy.md) | Rust |
| 09 Power, Thermal & Sleep Lifecycle | [09](verticals/09-power-thermal-sleep.md) | Go |
| 10 Desktop App Model (daemon / CLI / tray / gateway) | [10](verticals/10-desktop-app-model.md) | Go + Tauri |

**Owns in the contract:** settlement tx + zk/optimistic proof envelope; wallet WIT resource; lifecycle events (`THERMAL_SHED`/`SLEEP_IMMINENT`/`WAKE`); IPC gRPC service + operator-capability bootstrap; OpenAI-compatible gateway shape.

**Standalone build target:** the economy module, the lifecycle monitor, and the daemon+CLI+tray — each runnable against a **stub core**.

**Standalone test plan:** settle a mock `ComputeTask` (eUTXO tx + optimistic fraud-proof challenge path; assert slash on mismatch) → fetch IPLD weight chunks from a local blockstore → drive the lifecycle monitor from simulated power events (assert checkpoint + capability hand-back + standby-promotion signal) → run `cerberus up/status/run` against a stub daemon → render the tray topology/rings from stub telemetry → hit the gateway with an OpenAI-shaped request and assert dispatch call.

**Consumes (stub until integration):**
| Need | Stub | Real source |
|---|---|---|
| wallet/capability handles | stub cap kernel | A / 00 |
| completed `ComputeTask` to settle | mock task producer | A / 03 |
| scheduler lifecycle hooks | mock scheduler | A / 06 |
| telemetry for the tray | synthetic feed | B / 08 |

---

## 4. Integration Milestone Ladder

```
 ① CONTRACT FREEZE ──► ② STANDALONE GREEN ──► ③ PAIRWISE ──► ④ FULL-MESH E2E
   (done: schemas        each workstream        A↔B then        all three on one
    locked)              passes vs mocks +       A↔C wired        mesh; acceptance
                         ships a standalone      to real          = ARCHITECTURE §4
                         binary/lib              counterparts     walkthroughs
```

1. **Contract freeze** — schemas locked ([ARCHITECTURE.md §3](../ARCHITECTURE.md), [schemas.md](schemas/schemas.md)). Done.
2. **Standalone green** — each workstream's own test suite passes against its mocks; each ships a runnable standalone artifact. No cross-team dependency to reach this gate.
3. **Pairwise integration:**
   - **A ↔ B:** replace A's loopback transport with real Zenoh+libp2p; replace A's always-valid verifier with real 07 identity + `sys/revocations`; replace B's stub cap kernel with real 00. Assert: capability-gated topics, attested admission, real revocation propagation.
   - **A ↔ C:** feed C real completed `ComputeTask`s from 03; wire 09 lifecycle hooks into the real 06 scheduler; back C's wallet stub with real 00. Assert: settlement on real work, lid-drop → real re-route.
4. **Full-mesh E2E** — all three on one mesh. Acceptance tests are the three walkthroughs in [ARCHITECTURE.md §4](../ARCHITECTURE.md): *"2 GiB remote VRAM request"*, *"lid-drop mid-pipeline"*, *"Open Mesh cross-org trade"*.

---

## 5. Suggested Branch / Worktree Strategy

- One long-lived integration branch (`main`); three working branches `ws/core`, `ws/fabric`, `ws/economy` (or git worktrees of the same names for parallel checkouts).
- Each working branch is self-contained behind the contract; merges to `main` only require **standalone-green** + contract conformance, not the other workstreams.
- A change to the frozen contract is a dedicated PR reviewed by all three before any workstream adopts the new version.

---

## 6. At-a-Glance Ownership Map

| # | Vertical | Workstream |
|---|---|---|
| 00 | OCap Security Kernel | **A** |
| 01 | Mesh Fabric & Transport | **B** |
| 02 | Distributed State & CRDTs | **A** |
| 03 | Compute Orchestration | **A** |
| 04 | 9P Peripheral Virtualization | **A** |
| 05 | eUTXO & Open Mesh Economy | **C** |
| 06 | Placement & Scheduling Brain | **A** |
| 07 | Identity & Capability Lifecycle | **B** |
| 08 | Observability & Tracing | **B** |
| 09 | Power, Thermal & Sleep | **C** |
| 10 | Desktop App Model | **C** |
