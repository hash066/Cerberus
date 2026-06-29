# Agent Briefs — fire the swarm

Ready-to-paste prompts for the parallel build. **Phase 0 (the frozen contract + monorepo skeleton) is already committed** — every lane starts from a green `go build ./...` and `cargo build --workspace`. Each tool works on its **own branch/worktree** and touches **only its directories** ([CONTRACT.md §3](../CONTRACT.md)).

## Setup (run once)
```bash
# from repo root, after Phase 0 is committed on main
git branch ws/core ws/fabric ws/surface ws/platform   # (already created by the conductor)

# give each tool its own working copy (no collisions on one laptop):
git worktree add ../cerberus-core    ws/core
git worktree add ../cerberus-fabric  ws/fabric
git worktree add ../cerberus-surface ws/surface
git worktree add ../cerberus-platform ws/platform
```
Then open each worktree folder in its assigned tool.

## Lane → tool
| Lane | Tool | Branch / worktree | Verticals |
|---|---|---|---|
| A — Core | **Claude Code** (this session's worktree agents) | `ws/core` | 00, 02, 03, 04, 06 |
| B — Fabric | **Cursor (Pro)** | `ws/fabric` | 01, 07, 08 |
| C — Surface | **Antigravity (Pro)** | `ws/surface` | 05, 09, 10 |
| D — Platform | **Codex** | `ws/platform` | codegen, CI, E2E, examples, fixer |

---

## Shared preamble (prepend to EVERY brief)
```
You are working in the Cerberus monorepo (a zero-trust distributed hypervisor).
READ FIRST: CLAUDE.md, CONTRACT.md, ARCHITECTURE.md (esp. §3 schemas), docs/workstreams.md,
and your vertical docs in docs/verticals/.

HARD RULES:
- Do NOT edit the frozen contract: proto/, components/wit/, schemas/, contract/. If you need a
  contract change, STOP and request it (it is a cross-lane PR).
- Stay ONLY in your lane's directories (see CONTRACT.md §3).
- Build against the contract types in `contract/go` (Go) and `contract/rust` (Rust). For anything
  another lane owns, use the in-memory stubs in `contract/go/stub` (Go) — do not call other lanes directly.
- Green-or-it-didn't-happen: `go build ./...`, `go test ./...`, `cargo build`, `cargo test` must pass
  for your packages before you call anything done. Add ≥1 unit test per vertical.
- Frontier features stay documented stubs (zk-WASM proof-of-inference, RDMA, host-TEE). Do not fake them.
- Match existing code style. Keep the Go↔Rust boundary tiny (opaque u64 handles only).
```

---

## Brief B — Cursor (Fabric: 01 mesh, 07 identity, 08 observability)
```
<shared preamble>

Build Workstream B in branch ws/fabric. Owned dirs: daemon/mesh, daemon/telemetry,
daemon/supervisor, core/identity.

01 Mesh & Transport (daemon/mesh, Go): implement the contract.Fabric interface for real.
  - Zenoh intra-site pub/sub (eclipse-zenoh/zenoh, zenoh-go or c-binding) on keys
    cerberus/<site>/{telemetry,crdt,captp,sched}/*.
  - libp2p inter-site (libp2p/go-libp2p + go-libp2p-pubsub gossipsub, DCUtR) over quic-go.
  - mDNS discovery; QUIC sessions implementing contract.Session.
  - OTP-style supervision in daemon/supervisor using thejerf/suture.
  - Standalone test: 2 in-process Fabric instances discover + exchange a message; a supervised
    child restarts after a forced crash.
07 Identity (core/identity, Rust): extend the RevocationSet + attestation stub into real Ed25519
  keygen/rotation, gossiped sys/revocations OR-set, and the cap_verify/is_revoked seam used by core/ocap.
08 Observability (daemon/telemetry, Go): publish contract.NodeTelemetry on Zenoh at 1-4 Hz;
  OpenTelemetry-style spans (open-telemetry/opentelemetry-go); back-pressure-aware.
Stub everything from lane A (cap kernel) via contract/go/stub. DoD in CONTRACT.md §5.
```

## Brief C — Antigravity (Surface: 05 economy, 09 lifecycle, 10 desktop)
```
<shared preamble>

Build Workstream C in branch ws/surface. Owned dirs: core/economy, daemon/lifecycle,
daemon/gateway, tray/, cmd/.

05 Economy (core/economy, Rust): extend the eUTXO Ledger into optimistic settlement +
  fraud-proof re-execution + wallet WIT resource; IPLD weight fetch (ipld/go-ipld-prime on the Go side)
  + Reed-Solomon (klauspost/reedsolomon). zk-WASM = documented stub. Profile-gate everything OFF in Sealed.
09 Lifecycle (daemon/lifecycle, Go): power/thermal/sleep via shirou/gopsutil + OS APIs; emit
  THERMAL_SHED/SLEEP_IMMINENT/WAKE; capability hand-back + checkpoint hooks.
10 Desktop (cmd/, daemon/gateway, tray/): grow cmd/cerberusd + cmd/cerberus onto the gRPC-over-UDS
  IPC contract (ARCHITECTURE.md §3.8, talking to a stub core); OpenAI-compatible gateway in daemon/gateway;
  Tauri v2 tray (tauri-apps/tauri) rendering topology/telemetry from contract.TelemetrySource (stub feed).
Stub lane A (cap kernel, scheduler, executor) and lane B (telemetry) via contract/go/stub. DoD in CONTRACT.md §5.
```

## Brief D — Codex (Platform: codegen, CI, E2E, examples, fixer)
```
<shared preamble>

Build Workstream D in branch ws/platform. Owned dirs: proto/, components/, build/, .github/, test/.
(You MAY regenerate code into contract/*/gen but never hand-edit contract source.)

1. Codegen: install buf + protoc-gen-go + wit-bindgen; wire `task gen` to emit Go bindings from proto/
   into contract/go/gen and component bindings from components/wit/. Keep handwritten contract/ in sync.
2. CI: extend .github/workflows/ci.yml — matrix build/test for Go + Rust, golangci-lint, clippy -D warnings,
   cargo fmt --check, and run `go run ./test/e2e` as a smoke gate.
3. Example components: build components/examples/hello-shard (and a toy matmul) as real WASM components
   via cargo-component against components/wit/cerberus-agent.wit.
4. E2E: grow test/e2e from the in-process smoke into a real 2-process harness (spawn two cerberusd,
   discover, run hello-shard.wasm on the remote, assert result). Add as `task demo`.
5. Packaging: goreleaser config in build/; dev container.
6. Fixer swarm: keep all four branches compiling — triage build/test failures across lanes.
Many of these are independent — parallelize aggressively.
```

---

## Lane A — Claude Code (Core: 00, 02, 03, 04, 06)
Driven from the Claude session via worktree-isolated agents on `ws/core`. Each agent owns one vertical,
extends the v0.1 skeletons already in place:
- 00 `core/ocap`: MemKernel → real Ed25519/CBOR capabilities, attenuation chains, WIT host on wasmtime, CapTP-lite.
- 02 `core/crdt`: KvDoc → automerge/yrs wrapper, domain reducers, agent.belief contradiction flagging.
- 03 `core/runtime`: EchoExecutor → wasmtime component host (WASI P2), gpu-capability dispatch, promise pipelining.
- 04 `daemon/ninep`: 9P2000.L server (hugelgupf/p9) + go-fuse mount, cap-gated walk/open, ctl→endpoint.
- 06 `daemon/scheduler`: greedy constrained bin-packer over contract.TelemetrySource (stub), re-route + hot standby.

## Integration (conductor / `main`)
When lanes are standalone-green, merge `ws/platform` → `main`, then `ws/core` → `ws/fabric` → `ws/surface`,
wiring real seams and flipping the FFI on with `-tags ffi`. Acceptance = `task demo` (real 2-process path).
See [docs/workstreams.md §4](workstreams.md).
