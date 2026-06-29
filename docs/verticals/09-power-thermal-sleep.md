# Vertical 09 — Power, Thermal & Sleep Lifecycle (NEW)

> **Workstream C — Economy, Lifecycle & Surface.** Depends on: 06 (scheduler hooks), 02 (CRDT checkpoint), 01 (supervisor). Stub until integration: scheduler, CRDT engine. See [docs/workstreams.md](../workstreams.md).

> **Net-new vertical.** The target nodes are *laptops*: they throttle, sleep, and have their lids closed mid-pipeline. Volume I treated this as an edge case; Volume II makes node lifecycle a **first-class scheduler input**. Conforms to [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Purpose & Responsibilities
- Monitor and predict node lifecycle: AC/battery, battery %, thermal headroom/throttle, lid state, and imminent sleep.
- Feed these as constraints to the scheduler (06) and trigger **graceful capability hand-back** and **CRDT checkpointing** before a node goes dark.
- Resume cleanly on wake (re-advertise capabilities, merge state).

## 2. Position in the System
- **Control plane.** Produces `Power`/`Thermal` telemetry ([schemas §2](../schemas/schemas.md)); drives scheduler re-routing (06) and CRDT checkpoint (02).
- Upstream: OS power/thermal APIs. Downstream: scheduler, CRDT engine, supervisor.

## 3. Detailed Architecture
```
   OS power/thermal APIs ──► Lifecycle Monitor ──► Power/Thermal telemetry (08)
        (IOKit / Win32 /         │
         /sys/class/...)         ├─ THERMAL_SHED: signal scheduler(06) to shed load to cooler nodes
                                 │
                                 └─ SLEEP_IMMINENT:
                                      1. CRDT checkpoint (02) — seal vector clock, flush deltas
                                      2. hand back capabilities (00/07) — endpoints closed cleanly
                                      3. notify supervisor(01) — peer-down pre-announced
                                      4. scheduler(06) promotes hot standby BEFORE sleep
                                    ─────────────────────────────────────────────
                                    on WAKE: re-advertise caps, CRDT merge, rejoin mesh
```
- **Thermal management:** continuous tracking; on approaching limits the node emits `THERMAL_SHED` and the scheduler scales back its assignment, moving layers/blocks to cooler nodes (Volume I §6.3, now event-driven and capability-aware).
- **Sleep choreography:** the key improvement over Volume I's reactive "lid-drop" handling — sleep is usually *predictable* (idle timer, battery threshold, OS notification). The node pre-announces `SLEEP_IMMINENT`, checkpoints, and hands back capabilities **before** going dark, so the scheduler promotes a standby proactively instead of detecting a hard failure after the fact.
- **Battery policy:** on battery + low %, the node de-prioritizes itself in the cost model (06) and may refuse new alloc capabilities.

## 4. Data Structures / Wire Formats
- `Power`, `Thermal` in `NodeTelemetry` ([schemas §2](../schemas/schemas.md)).
- Lifecycle events: `THERMAL_SHED`, `SLEEP_IMMINENT`, `WAKE` ([schemas §8](../schemas/schemas.md)) published on `cerberus/<site>/lifecycle/<peer>`.
- Checkpoint blob: `doc_checkpoint()` output from [02](02-distributed-state-crdts.md).

## 5. Interfaces / APIs (Go)
```go
type Lifecycle interface {
    State() PowerState                          // src, battery, lid, thermal, sleepHint
    OnTransition(func(ev LifecycleEvent))       // THERMAL_SHED | SLEEP_IMMINENT | WAKE
    PrepareSleep() error                        // checkpoint + hand-back + pre-announce
    Resume() error                              // re-advertise + merge + rejoin
}
```

## 6. Tech Stack
| Concern | Choice | Why |
|---|---|---|
| macOS | IOKit / IOPMowerSource, thermal pressure API | native power/thermal signals |
| Windows | Win32 Power Management (`GetSystemPowerStatus`, power notifications) | lid/battery/sleep events |
| Linux | `/sys/class/power_supply`, `/sys/class/thermal`, systemd-logind inhibitors | power/thermal + sleep inhibit |
| Integration | scheduler (06), CRDT (02), supervisor (01) | first-class lifecycle inputs |

## 7. Security Model
- Capability hand-back is clean: endpoints granted to a sleeping node are revoked/closed, preventing dangling authority.
- Lifecycle events are capability-gated telemetry (no node learns another's power state without a grant).

## 8. Open Mesh vs Sealed
- **OpenMesh:** a node may monetize "awake + AC + thermal-headroom" availability windows ([05](05-eutxo-open-mesh-economy.md)).
- **Sealed:** lifecycle events audited; servers (always-on, AC) preferred for critical shards.

## 9. Failure Modes & Mitigations
| Failure | Mitigation |
|---|---|
| Unpredicted hard sleep (lid slam) | falls back to supervisor (01) peer-down + hot-standby promotion; CRDT merges on wake |
| Thermal runaway | proactive `THERMAL_SHED` before throttle floor |
| Battery dies mid-task | low-battery de-prioritization + standby duplication |
| Wake storm (many nodes resume) | staggered re-advertise + back-pressured telemetry (08) |

## 10. Verdict
**Shippable.** OS power/thermal APIs are well-understood; the value is treating them as first-class. Open question: predictive sleep model (learn user idle patterns) for even earlier standby promotion — a nice-to-have, not required.
