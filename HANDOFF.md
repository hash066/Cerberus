# Cerberus — Session Handoff / Compact Context

> Read this first in a new session. It is the single source of "where things actually stand" — what is real, what is stubbed, what is blocked, and exactly how to continue. Honest by design.

## What Cerberus is
A zero-trust **distributed hypervisor for multi-agent orchestration**: bind heterogeneous machines (Macs/PCs/Linux) into a local, capability-secured mesh that runs autonomous agent swarms — escaping centralized cloud. **Desktop app** (headless daemon `cerberusd` + CLI `cerberus` + Tauri tray UI). Go = control plane; Rust = capability kernel / WASM / crypto / CRDT. Full vision in [ideadumpp2.md](ideadumpp2.md); canonical spec in [ARCHITECTURE.md](ARCHITECTURE.md); per-vertical designs in [docs/verticals/](docs/verticals/).

## Current state (truth)
- Branch `main` (and `integration`) at the latest commit; everything below is committed + pushed to `github.com/hash066/Cerberus`.
- **Green:** `go build/vet/test ./...`, `cargo build --workspace`, `cargo test --workspace`, `cargo fmt --check`, `cargo clippy -D warnings`.
- **Demo passes:** `go run ./test/e2e` → two `cerberusd` processes discover each other and run `hello_shard.wasm` remotely via **real wazero** → returns `1337`.

### Build / run / test (commands)
```
go build ./...              # build daemon/CLI/all Go
go test ./...               # all Go tests
cargo build --workspace     # Rust core crates (tray + wasm examples are excluded)
cargo test --workspace
go run ./test/e2e           # the 2-node remote-WASM acceptance demo (prints 1337)
# desktop UI (needs Tauri toolchain + WebView2, NOT verified in this env):
#   run cmd/cerberusd, then:  cd tray && cargo tauri dev
```
Commits: author/committer = **hash066 <harshitanagesh4@gmail.com>** (use `git commit --author=...`). Push to `main` is allowed (done throughout). `integration` mirrors `main`.

## Reality matrix (REAL vs STUB vs BLOCKED)
| Area | Status |
|---|---|
| Real WASM execution (Go: `daemon/wasm` via **wazero**; Rust: `core/runtime` via **wasmi**) | ✅ REAL, tested |
| OCap capability tokens — Ed25519-signed, gateway+RPC+API gated, multi-user (`daemon/auth`) | ✅ REAL, tested |
| Durable persistence — bbolt store; issuer key, revocations, eUTXO ledger, CRDT checkpoints survive restart (`daemon/store`,`daemon/ledger`,`daemon/state`,`daemon/auth`) | ✅ REAL, tested |
| Status API + Tauri dashboard (`daemon/api`, `tray/`) | ✅ API REAL+tested; GUI real code, **not launched here** (needs Tauri toolchain) |
| Composed daemon (OCap + mesh + telemetry + scheduler + 9P under OTP supervisor) (`daemon/system`) | ✅ REAL (composition); mesh = real libp2p/QUIC |
| Scheduler — cost-model placement, reroute/standby, **multi-shard pipeline placement** (`daemon/scheduler`) | ✅ REAL, tested |
| CRDT belief-conflict flagging (`core/crdt`, `daemon/state`) | ✅ REAL, tested |
| OCap kernel in Rust — Ed25519 CBOR caps + attenuation chains (`core/ocap` SignedKernel) | ✅ REAL, tested — **but NOT bound into the Go daemon** (see blocked) |
| Real Rust kernel via cgo (`daemon/ffi` `-tags ffi` → `core/cabi`) | ⛔ BLOCKED: no C toolchain here (`CGO_ENABLED=0`). Default daemon uses a pure-Go stub kernel. Code path exists behind the `ffi` build tag. |
| mTLS / PeerID on the wire | ⛔ TODO (RPC/gateway are token-gated but plain TCP/localhost) |
| zk-WASM proof-of-inference, host-TEE memory shielding, RDMA-over-Thunderbolt | ⛔ FRONTIER (documented stubs by design; ~100× / hardware-limited) |
| eUTXO advanced settlement (fraud proofs/zk) in `core/economy` (Rust) | ⚠️ model exists; daemon's live ledger is the Go `daemon/ledger` (durable) |
| Tauri GUI rendering | ⛔ not verifiable headlessly; the preview of `tray/src/index.html` in a plain browser looks unstyled because CSS+Tauri runtime aren't loaded there — it is NOT the real app |

## Phases completed
- **Real WASM core** — replaced a hand-rolled i32-parser with wazero (Go) / wasmi (Rust); gateway runs real WASM.
- **Phase B — multi-user auth**: `daemon/auth` Ed25519 bearer capability tokens (mint/authorize/attenuate/revoke, expiry, scope, admin); enforced on gateway (`:8080`), RPC (`:9092`), status API (`:7777`); operator token written to OS config dir; CLI authenticates.
- **Phase C — durability**: bbolt `daemon/store`; persisted issuer key + revocations; durable eUTXO `daemon/ledger`; durable CRDT `daemon/state` (LWW map + checkpoints + belief-conflict). All survive restart; wired into `cerberusd`.
- **Phase D — desktop dashboard**: `daemon/api` token-gated status JSON; `cerberusd` serves live data on `:7777`; `tray/` real Tauri v2 dashboard (Rust does the authenticated fetch, webview renders — no browser).
- **Phase E (started)**: multi-shard pipeline placement in the scheduler.

## Repo layout (key)
```
cmd/cerberusd        Go daemon (composes everything; serves gateway:8080, rpc:9092, api:7777)
cmd/cerberus         Go CLI (reads operator token; talks RPC)
daemon/  auth ffi gateway api store ledger state system scheduler ninep mesh telemetry supervisor lifecycle economy
core/    (Rust) ocap crdt runtime identity economy cabi   [cabi = C-ABI for cgo, behind -tags ffi]
contract/ go + rust  FROZEN integration types (see CONTRACT.md) — do not edit casually
proto/ components/wit/ schemas/   frozen contract sources
tray/    Tauri v2 desktop app (src = webview UI, src-tauri = Rust shell)  [excluded from cargo workspace]
test/e2e Go 2-process demo harness (+ test/e2e/node)
docs/    ARCHITECTURE refs, verticals/, workstreams.md, agent-briefs.md
```
**Frozen contract:** `contract/`, `proto/`, `components/wit/`, `schemas/` are the integration seam (see [CONTRACT.md](CONTRACT.md)). Change only via a deliberate contract change, not casually.

## Key decisions / constraints
- **No cgo on this machine** (`CGO_ENABLED=0`, Go win/386, no C compiler) → the real Rust OCap kernel isn't bound; daemon uses the pure-Go stub kernel. Binding it (`-tags ffi`) is the #1 thing needing a dev box with gcc/clang.
- **No browser**: UI is Tauri native webview; Rust performs network calls, webview only renders.
- **Two profiles, one binary**: `open_mesh` (eUTXO economy ON) vs `sealed` (chain OFF, attestation ON). `cerberusd -profile=...`.
- **Pure-Go choices to avoid toolchain blocks**: wazero (WASM), bbolt (KV), Ed25519 (stdlib).
- Each increment is converted for real, verified green, and pushed; unverifiable parts are called out, not faked.

## Phase E roadmap (what's left — prioritized)
1. **Bind real Rust OCap kernel via cgo** (`-tags ffi`) once a C toolchain is available — makes capabilities cryptographically enforced at runtime end-to-end.
2. **Real cross-node compute**: move the E2E demo transport from HTTP onto the composed mesh; resolve `ComputeTask.Component` CID → wasm bytes via IPLD; network promise pipelining.
3. **Runtime scale**: wasmtime Component Model + WASI P2 (richer than wazero), `gpu` capability dispatch (MLX / wgpu), zero-copy/RDMA data plane.
4. **Scheduler depth**: real telemetry-driven cost model in the live daemon; tensor sharding; multi-objective optimizer.
5. **Hardening**: mTLS + PeerID on the wire; persisted/rotating issuer key custody (Secure Enclave/TPM); rate limits/quotas enforced on the data plane.
6. **Ops**: real OTel tracing exported; metrics/health; **load tests** (many nodes, concurrent users) + **chaos tests** (partition, lid-drop, node loss) as automated suites; security audit/fuzzing.
7. **Frontier** (documented stubs): zk-WASM, host-TEE, RDMA-over-Thunderbolt.

## Where to pick up
Start a new session with: *"Read HANDOFF.md, CONTRACT.md, ARCHITECTURE.md. Continue Phase E item N (…)."* Pattern that has worked: pick one item, implement it for real, keep `go/cargo build+test` green + `go run ./test/e2e` passing, commit as hash066, push `main`. Keep the frozen contract frozen.
