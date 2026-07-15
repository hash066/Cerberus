# AGENTS.md — Cerberus build conventions (read me first)

Cerberus is a zero-trust distributed hypervisor for multi-agent orchestration. The canonical spec is [ARCHITECTURE.md](ARCHITECTURE.md); per-vertical low-level designs are in [docs/verticals/](docs/verticals/); the work is split into lanes in [docs/workstreams.md](docs/workstreams.md). This file tells any agent (Codex, Cursor, Antigravity, Codex) how to work here without colliding.

## Golden rules
1. **Never edit the frozen contract unilaterally.** `proto/`, `components/wit/`, `schemas/`, and the generated/handwritten contract types in `contract/` are the integration seam. Changing them is a cross-lane event — see [CONTRACT.md](CONTRACT.md).
2. **Stay in your lane's directories.** Ownership is in [docs/workstreams.md](docs/workstreams.md) and [CONTRACT.md](CONTRACT.md). Do not touch another lane's dirs.
3. **Stub what you consume.** Anything from another lane is mocked behind the contract until integration. Each lane must build and unit-test standalone.
4. **Green or it didn't happen.** A change is done only when `task build` and `task test` pass for your packages.
5. **Capabilities, not identities.** No ambient authority anywhere — every cross-boundary call presents a capability handle. See [docs/verticals/00-ocap-security-kernel.md](docs/verticals/00-ocap-security-kernel.md).

## Layout
```
contract/   frozen contract: Go pkg (contract/go) + Rust crate (contract/rust)   [DO NOT edit casually]
proto/      protobuf source of truth (telemetry, crdt op, compute task)          [DO NOT edit casually]
components/ WIT worlds + example WASM components                                  [wit/ DO NOT edit casually]
schemas/    CDDL (capability, settlement) + JSON-schema (profile)                 [DO NOT edit casually]
core/       Rust crates: ocap, crdt, runtime, identity, economy, cabi (FFI)
daemon/     Go packages: mesh, ninep, scheduler, telemetry, lifecycle, gateway, supervisor
cmd/        Go binaries: cerberusd (daemon), cerberus (CLI)
tray/        Tauri v2 system-tray app
test/       integration + E2E (the 2-node demo harness)
build/      packaging, cross-compile, CI helpers
```

## Languages & boundaries
- **Go** owns the control plane / daemon / CLI / scheduler / mesh / 9P. One Go module: `github.com/hash066/cerberus`.
- **Rust** owns the capability kernel / WASM runtime / crypto / CRDT engine / economy. One Cargo workspace at repo root.
- **Go ↔ Rust** meet only at `core/cabi` (C-ABI, opaque `u64` capability handles). Keep this boundary tiny.
- **Host ↔ guest** meet only at the WIT world in `components/wit/`. A guest sees only its imported capabilities.

## Commands
- `task build` — build Go + Rust. `task test` — unit tests. `task lint` — golangci-lint + clippy. `task demo` — 2-node WASM-exec demo. (If `task` is not installed: `go install github.com/go-task/task/v3/cmd/task@latest`, or run the raw `go`/`cargo` commands in [Taskfile.yml](Taskfile.yml).)

## Maturity honesty
Per [ARCHITECTURE.md §8](ARCHITECTURE.md), some pieces are **Frontier** (zk-WASM proof-of-inference, RDMA-over-Thunderbolt, host-TEE memory shielding). For v0.1 these are **documented stubs**, not real implementations. Do not fake them as working.
