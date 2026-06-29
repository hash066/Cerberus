# CONTRACT.md — The Frozen Integration Contract

> 🔒 This document governs the shared seam every lane builds against. It is the reason four tools can work in parallel without colliding. The normative schema definitions live in [ARCHITECTURE.md §3](ARCHITECTURE.md) and [docs/schemas/schemas.md](docs/schemas/schemas.md); this file is the engineering protocol around them.

## 1. What is frozen
- `proto/cerberus/v1/*.proto` — telemetry, CRDT op, compute task (Protobuf source of truth).
- `components/wit/*.wit` — the `caps` world (host↔guest capability interface).
- `schemas/*.cddl` + `schemas/*.json` — capability + settlement (CBOR/CDDL), profile config (JSON-schema).
- `contract/go/` and `contract/rust/` — the **v0.1 compiling contract types** that mirror the above.

### v0.1 codegen note (important)
`buf` / `protoc` / `wit-bindgen` are **not assumed installed**. So for v0.1 the **handwritten types in `contract/go` and `contract/rust` are the buildable contract**, and the `.proto`/`.wit` files are the canonical spec they mirror. When the codegen toolchain is available, `task gen` will regenerate bindings and the handwritten types are replaced. Until then: **keep `contract/go` and `contract/rust` in sync with the `.proto`/`.wit` by hand, and only via a contract PR.**

## 2. How to change the contract
1. Open a PR that touches only `contract/`, `proto/`, `components/wit/`, or `schemas/`.
2. Tag all four lanes for review.
3. Bump the contract version (`ContractVersion` in `contract/go/version.go` and `contract/rust/src/lib.rs`).
4. Merge to `main`; lanes rebase and adopt.

A lane that needs a contract change **does not edit it locally** — it requests the change. This is the single rule that prevents drift.

## 3. Directory ownership (no two lanes share a file)
| Lane | Branch | Owns |
|---|---|---|
| **A — Core** (Claude) | `ws/core` | `core/ocap`, `core/crdt`, `core/runtime`, `daemon/ninep`, `daemon/scheduler` |
| **B — Fabric** (Cursor) | `ws/fabric` | `daemon/mesh`, `core/identity`, `daemon/telemetry`, `daemon/supervisor` |
| **C — Surface** (Antigravity) | `ws/surface` | `core/economy`, `daemon/lifecycle`, `daemon/gateway`, `tray/`, `cmd/` |
| **D — Platform** (Codex) | `ws/platform` | `proto/`, `components/`, `build/`, `.github/`, `test/`, root tooling |
| Conductor (this session) | `main` | contract authorship, integration, review |

> Adding a dependency edits root `go.mod`/`Cargo.toml` — coordinate trivial merges; prefer `go get`/`cargo add` then commit just the lockfile delta.

## 4. The seams (what each lane stubs until integration)
| Lane | Consumes | Stub it uses standalone | Real source |
|---|---|---|---|
| A | telemetry feed; transport; identity verifier | synthetic telemetry; in-process loopback; always-valid verifier | B |
| B | `cap_verify`/`is_revoked`; CRDT engine | stub cap kernel; local CRDT instance | A |
| C | wallet/cap handles; completed `ComputeTask`; scheduler hooks | stub cap kernel; mock task producer; mock scheduler | A |
| D | (none — produces codegen/CI/harness all lanes consume) | — | — |

## 5. Definition of done (per lane, before merge to main)
- Builds: `task build` green for your packages.
- Tests: `task test` green; each owned vertical has ≥1 unit test.
- Lint: `task lint` clean (golangci-lint / clippy).
- Standalone: runs against stubs without the other lanes present.
- Docs: your vertical doc's §5 interfaces match your code.

## 6. The acceptance test (whole system)
`task demo` → two `cerberusd` instances discover each other → `cerberus run hello-shard.wasm --on <peer>` → the capability-gated WASM task executes on the remote node → result returns. This is the v0.1 finish line ([ARCHITECTURE.md §4.1](ARCHITECTURE.md)).
