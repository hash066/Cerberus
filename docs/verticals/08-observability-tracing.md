# Vertical 08 — Observability & Distributed Tracing (NEW)

> **Workstream B — Connectivity & Trust Fabric.** Depends on: 01 (Zenoh transport). Stub until integration: none (self-contained collector). See [docs/workstreams.md](../workstreams.md).

> **Net-new vertical.** A zero-trust mesh you cannot trace is unauditable — fatal for the Sealed profile and painful for debugging the Open one. Conforms to [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Purpose & Responsibilities
- Collect **telemetry** (the metrics already defined in [schemas §2](../schemas/schemas.md)) and **distributed traces** of agent workflows, capability flows, scheduling decisions, and (OpenMesh) settlement proofs.
- Feed the scheduler (06), lifecycle (09), and tray UI (10).
- Provide **audit export** for Sealed deployments.

## 2. Position in the System
- **Control plane**, cross-cutting. Produces the data every other control vertical consumes.
- Upstream: every vertical emits spans/metrics. Downstream: scheduler, tray, audit.

## 3. Detailed Architecture
```
   each vertical ──emit──► Span/Metric ──► local collector ──► Zenoh: cerberus/<site>/otel/*
                                                  │
                  ┌───────────────────────────────┼───────────────────────────────┐
                  ▼                                ▼                               ▼
            Scheduler (06)                  Tray UI (10)                    Audit Sink (Sealed)
         (cost-model inputs)            (topology, rings, thermals)     (append-only, retention)
```
- **Spans** follow a workflow across nodes: a task's span tree links the originating agent → scheduler placement → each shard's execution (03) → 9P grants (04) → settlement (05). **Trace context propagates with CapTP frames** so a capability's journey is reconstructable.
- **Metrics:** the `NodeTelemetry` packet at 1–4 Hz (back-pressure-aware: under poor Wi-Fi, Zenoh batches/drops telemetry before it blocks data — Volume I's bandwidth primitive).
- **Capability-flow tracing:** because authority travels with references, the trace doubles as a *capability provenance graph* — invaluable for audit ("which agent touched this VRAM, under what grant?").
- **Sampling:** head-based for hot paths, tail-based retention of error/anomaly traces.

## 4. Data Structures / Wire Formats
- OTel-style span: `{trace_id, span_id, parent, name, attrs:{cap_id, peer, shard,...}, start, end}` (OTLP-compatible, CBOR on the wire).
- Metrics: `NodeTelemetry` ([schemas §2](../schemas/schemas.md)).
- `belief.conflict` events from [02](02-distributed-state-crdts.md) are first-class observability events.

## 5. Interfaces / APIs (Go)
```go
type Tracer interface {
    StartSpan(name string, ctx TraceCtx, attrs Attrs) (Span, TraceCtx)
    Inject(ctx TraceCtx, frame *CapTPFrame)     // propagate across nodes
    Extract(frame *CapTPFrame) TraceCtx
}
type Metrics interface { Emit(t NodeTelemetry); Subscribe(keyExpr string) (<-chan NodeTelemetry, error) }
```

## 6. Tech Stack
| Concern | Choice | Why |
|---|---|---|
| Tracing model | **OpenTelemetry**-style spans (OTLP) | standard, tool-compatible |
| Transport | Zenoh pub/sub ([01](01-mesh-fabric-transport.md)) | low overhead, back-pressure-aware |
| Surfacing | Tauri tray ([10](10-desktop-app-model.md)) | native, no browser |
| Audit sink (Sealed) | append-only log + retention | compliance |

## 7. Security Model
- Telemetry/trace streams are **capability-gated** topics — a node cannot subscribe to another's traces without a grant.
- Audit export (Sealed) is integrity-protected (hash-chained log); optional PII redaction on span attributes.
- Trace data never carries secured payload bytes — only identifiers (cap ids, CIDs, peer ids).

## 8. Open Mesh vs Sealed
- **OpenMesh:** local + opt-in trace sharing for debugging; minimal retention.
- **Sealed:** mandatory audit export, configurable retention, redaction; capability-provenance graph retained for compliance.

## 9. Failure Modes & Mitigations
| Failure | Mitigation |
|---|---|
| Telemetry storms saturate Wi-Fi | Zenoh batches/drops telemetry under back-pressure before blocking data |
| Trace gaps across partition | spans buffered locally, stitched on reconnect via trace_id |
| Sensitive data in spans | attribute allow-list + redaction (Sealed) |
| Clock skew distorts spans | logical ordering via vector clocks where causal precision matters |

## 10. Verdict
**Shippable.** OTel + Zenoh is a well-trodden combination. Open question: standard schema for the capability-provenance graph so external SIEM tools can ingest it.
