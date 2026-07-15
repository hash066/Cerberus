# Observability gaps (production-hardening audit)

Audit scope: `daemon/metrics`, `daemon/telemetry`, `daemon/api`, `cmd/cerberusd`.
No contract/proto changes. Status as of the production-hardening pass.

## Implemented (v0.1)

| Surface | Location | Notes |
|---------|----------|-------|
| `/metrics` | `daemon/metrics` | Prometheus text exposition; Bearer token with `read` right |
| `/healthz` | `daemon/metrics`, `daemon/api` | Liveness; unauthenticated on localhost bind |
| `/readyz` | `daemon/metrics` | Readiness; mesh-up predicate wired in `cmd/cerberusd` |
| `/api/v1/status` | `daemon/api` | Dashboard snapshot: subsystems, metrics summary, mesh |
| Fabric telemetry | `daemon/telemetry` | 1–4 Hz `cerberus/<site>/telemetry/<peer>` publish + OTel spans |
| Workload history | `cmd/cerberusd/workloads.go` | In-memory ring → `GET /api/v1/workloads` |

## Gaps (acceptable v0.1, blockers for production-grade)

### Metrics

- **Counters not fed at all call sites.** `Metrics` defines TasksPlaced, GatewayRequests, WasmExecsTotal, etc., but several producers still omit increments — dashboard `/api/v1/status` metrics summary stays at zero under real load.
- **No histograms.** Latency (RPC, gateway, WASM exec, mesh round-trip) is untracked; SLO alerting impossible.
- **No process/runtime metrics.** Go memstats, goroutine count, GC — not exported.
- **Single-node only.** No federation or push gateway for multi-node fleet view.

### Health / readiness

- **`/readyz` is mesh-only.** Does not gate on bbolt store open, issuer init, or supervisor subsystems — a half-dead daemon can report ready.
- **Duplicate `/healthz`** on API and metrics ports; orchestrators must pick one; no documented canonical probe path in manifest beyond `metrics_addr`.
- **No deep health.** No subsystem-specific `/healthz/<name>` or structured JSON probe body.

### Tracing

- **OTel exporter not wired in production path.** Spans are created (`telemetry.publish`) but no OTLP/Jaeger exporter configured in `cmd/cerberusd` — traces are in-process/no-op unless a future wiring lands.
- **No trace propagation** across mesh RPC or gateway → executor boundary.

### Workload history

- **In-memory only; lost on restart.** Documented v0.1 posture but not durable for ops/audit.
- **Gateway chat path may not record.** Tray "Run workload" via `/v1/chat/completions` can return empty without hitting `recordWorkload` — history stays empty (see HANDOFF.md #1).
- **No pagination or retention policy** beyond fixed ring capacity.

### Telemetry stream

- **Lossy by design** (newest-wins drops under back-pressure) — fine for live dashboard, unsuitable for audit/replay.
- **No subscriber in tray/CLI** for live fabric telemetry key; only tests and future aggregators would consume it.

## Recommended next steps (minimal, no feature creep)

1. Wire metric increments at scheduler, gateway, and wasm exec call sites (typed handles already exist).
2. Document canonical probe URLs in `daemon/discovery` manifest (`healthz_url`, `readyz_url`).
3. Extend `/readyz` to include store + issuer readiness from `daemon/system`.
4. Add OTLP exporter env vars (`OTEL_EXPORTER_OTLP_ENDPOINT`) with no-op default.
5. Fix gateway → executor path so workload history and WasmExecsTotal reflect tray/Run traffic.
