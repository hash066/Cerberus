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
# real Rust OCap kernel over cgo (Phase E item 1; needs zig + gnu target, see docs/ffi.md):
pwsh build/ffi.ps1 -Action test   # → daemon/ffi tests pass against the Ed25519 SignedKernel
pwsh build/ffi.ps1 -Action build  # → cerberusd-ffi.exe (boots with kernel=rust-signed-cabi)
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
| Scheduler — cost-model placement, reroute/standby, **multi-shard pipeline placement**, **per-node reroute (lid-drop)** (`daemon/scheduler`) | ✅ REAL, tested |
| CRDT belief-conflict flagging (`core/crdt`, `daemon/state`) | ✅ REAL, tested |
| Distributed revocation — OR-set engine (`core/crdt`) + **mesh gossip propagation** (`daemon/auth` `RevocationGossip`, wired in `cerberusd`) + attested admission (`core/identity`) | ✅ REAL, tested (Phase E4): revoke on A → published over cap-gated `sys/revocations` topic → applied on B. Rust OR-set also reachable via cgo. |
| Content-addressed component store (CID→wasm) — Go side (`daemon/wasm`) + Rust engine (`core/runtime`) + promise pipelining | ✅ REAL, tested (Phase E2). The e2e dispatches by CID over the mesh; Rust CID store + promises reachable via cgo. |
| Real cross-node compute over the mesh (`daemon/mesh` `compute.go`) | ✅ REAL, tested (Phase E2): the e2e remote WASM exec runs over a capability-gated libp2p/QUIC stream (HTTP exec path removed); `go run ./test/e2e` → 1337 over mesh. |
| Lid-drop lifecycle (`daemon/lifecycle`) → scheduler standby promotion, wired in `cerberusd` | ✅ REAL state machine + wiring, tested; real OS power/thermal hooks are a labelled stub |
| OCap kernel in Rust — Ed25519 CBOR caps + attenuation chains (`core/ocap` SignedKernel) | ✅ REAL, tested — **now bound into the Go daemon** under `-tags ffi` |
| Real Rust kernel via cgo (`daemon/ffi` `-tags ffi` → `core/cabi` → `SignedKernel`) | ✅ REAL, tested (Phase E item 1). Full `CapKernel` (mint/attenuate/verify/revoke) over cgo; `cerberusd -tags ffi` boots with `kernel=rust-signed-cabi`. Toolchain recipe in [docs/ffi.md](docs/ffi.md) (zig cc + `x86_64-pc-windows-gnu` + GOARCH=amd64). **Default** build is still the pure-Go stub on win/386 (no C toolchain needed). |
| mTLS / PeerID on the wire | 🟡 PeerID-bound sessions now enforced on the libp2p/QUIC mesh path + cap-gated topics (Phase E3, `daemon/mesh`); full custom-cert mTLS over a raw (non-libp2p) data-plane transport still TODO; local RPC/API remain token-gated localhost |
| Wasmtime component-model runtime (`core/runtime` `wasmtime_exec`, optional `wasmtime` feature) | ✅ REAL, tested (Phase F1) — alongside wasmi; runs a real component (fixture→1337). WASI-P2 = documented hook. Feature-gated so the cgo gnu staticlib opts out. |
| QUIC zero-copy data plane (`daemon/dataplane`) | ✅ REAL, tested (Phase F3) — capability+quota-bound bulk transfer (rejects over-quota up-front and mid-stream). Data-plane PeerID-pin = stub. |
| 9P2000.L wire server (`daemon/ninep` via hugelgupf/p9) | ✅ REAL, tested (Phase F4) — serves the cap-gated namespace; per-connection capability; `ctl`→endpoint invariant held. FUSE/WinFsp mount = labelled stub. |
| Network audio transport — mic/speaker (`daemon/audio`) | ✅ REAL, tested (Phase F5) — packetized sender/receiver, reordering jitter buffer, DLL drift control, gap-fill. OS capture (CoreAudio/WASAPI/PipeWire) = labelled stub. |
| GPU dispatch (wgpu/MLX) | ⛔ Phase F2 — deferred (needs real GPU hardware to validate; not faked). |
| zk-WASM proof-of-inference, host-TEE memory shielding, RDMA-over-Thunderbolt | ⛔ FRONTIER (documented stubs by design; ~100× / hardware-limited) |
| eUTXO advanced settlement (fraud proofs/zk) in `core/economy` (Rust) | ⚠️ model exists; daemon's live ledger is the Go `daemon/ledger` (durable) |
| Tauri GUI rendering | ⛔ not verifiable headlessly; the preview of `tray/src/index.html` in a plain browser looks unstyled because CSS+Tauri runtime aren't loaded there — it is NOT the real app |

## Phases completed
- **Real WASM core** — replaced a hand-rolled i32-parser with wazero (Go) / wasmi (Rust); gateway runs real WASM.
- **Phase B — multi-user auth**: `daemon/auth` Ed25519 bearer capability tokens (mint/authorize/attenuate/revoke, expiry, scope, admin); enforced on gateway (`:8080`), RPC (`:9092`), status API (`:7777`); operator token written to OS config dir; CLI authenticates.
- **Phase C — durability**: bbolt `daemon/store`; persisted issuer key + revocations; durable eUTXO `daemon/ledger`; durable CRDT `daemon/state` (LWW map + checkpoints + belief-conflict). All survive restart; wired into `cerberusd`.
- **Phase D — desktop dashboard**: `daemon/api` token-gated status JSON; `cerberusd` serves live data on `:7777`; `tray/` real Tauri v2 dashboard (Rust does the authenticated fetch, webview renders — no browser).
- **Phase E (in progress)**: multi-shard pipeline placement in the scheduler.
  - **E1 done — real Rust OCap kernel bound via cgo** (`-tags ffi`): the expanded C-ABI in `core/cabi` backs the full `contract.CapKernel` over the Ed25519 `SignedKernel`; capabilities are cryptographically enforced end-to-end. Default build unchanged (pure-Go stub, win/386). See [docs/ffi.md](docs/ffi.md).
  - **E2/E3/E4 + Track C landed via 4 parallel agents** (isolated worktrees, integrated on `integration`, all green + e2e=1337):
    - **E2** ✅: SHA-256 CIDv1 content store + CapTP promise pipelining (`core/runtime`); the e2e now dispatches the remote WASM exec **over the mesh by CID** (`daemon/mesh/compute.go`, `daemon/wasm/cidstore.go`) — HTTP exec path removed, still returns 1337.
    - **E3** ✅ (`daemon/mesh`,`daemon/telemetry`): PeerID-bound sessions + capability-gated pub/sub topics, used by the composed daemon.
    - **E4** ✅ (`core/crdt`,`core/identity`,`daemon/auth`): `sys/revocations` OR-set + **mesh gossip** (`RevocationGossip`, wired in `cerberusd`) so revoke-on-A denies-on-B; signed-challenge admission (TEE extension point).
    - **FFI engine bridges** ✅ (`core/cabi`,`daemon/ffi`): the Rust CID store + OR-set are reachable over cgo under `-tags ffi` (8/8 ffi tests).
    - **Track C** (`daemon/lifecycle`,`daemon/gateway`): event-driven lid-drop state machine + gateway input hardening; lid-drop wired in `cerberusd` to `scheduler.RerouteNode` (real OS power hooks still a stub).
  - **Phase E essentially closed.** Remaining E hardening: cross-kernel *signed* capability transfer on the wire (compute cap currently travels under a shared-kernel demo model), Zenoh intra-site, real OS power hooks.
- **Phase F (MLP) — landed via 4 parallel agents** (isolated worktrees, integrated on `integration`, all green + e2e + ffi):
  - **F1** ✅ Wasmtime component-model executor (`core/runtime/wasmtime_exec`, optional feature) alongside wasmi; WASI-P2 = hook.
  - **F3** ✅ Capability+quota-bound QUIC data plane (`daemon/dataplane`) — the bulk-byte path the 9P `ctl` open hands out.
  - **F4** ✅ 9P2000.L wire server (`daemon/ninep`) over the cap-gated namespace; FUSE/WinFsp mount = stub.
  - **F5** ✅ Network audio transport (`daemon/audio`) — the mic/speaker path; OS capture = stub.
  - **F2 deferred:** GPU dispatch (needs real GPU hardware to validate honestly).
  - **Next (cross-cut wiring):** 9P `ctl` open → `dataplane.RegisterGrant` → hand back the real endpoint (ARCHITECTURE §4.1); audio rides the data plane; serve the 9P wire server + a data-plane listener under the daemon supervisor. Then Phase F2 (GPU) on real hardware.

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
- **cgo path now works** (Phase E item 1): the default build is still pure-Go on win/386 (`CGO_ENABLED=0`, no C compiler needed), but `-tags ffi` binds the real Rust kernel using **zig cc** (GCC-compatible; cgo can't drive MSVC `cl.exe`), the Rust **`x86_64-pc-windows-gnu`** staticlib, and **GOARCH=amd64**. One quirk handled by `build/ffi.ps1`: strip the duplicate `__chkstk_ms` object (zig's `compiler_rt` vs Rust's `compiler_builtins`) and link `-lunwind`. Full recipe: [docs/ffi.md](docs/ffi.md).
- **No browser**: UI is Tauri native webview; Rust performs network calls, webview only renders.
- **Two profiles, one binary**: `open_mesh` (eUTXO economy ON) vs `sealed` (chain OFF, attestation ON). `cerberusd -profile=...`.
- **Pure-Go choices to avoid toolchain blocks**: wazero (WASM), bbolt (KV), Ed25519 (stdlib).
- Each increment is converted for real, verified green, and pushed; unverifiable parts are called out, not faked.

## Phase E roadmap (what's left — prioritized)
1. ✅ **DONE — Bind real Rust OCap kernel via cgo** (`-tags ffi`): capabilities are cryptographically enforced at runtime end-to-end (`core/cabi` → `SignedKernel`). Recipe + reproducible build in [docs/ffi.md](docs/ffi.md) / `build/ffi.ps1`. Remaining hardening: rotate the issuer key into custody (TPM/Secure Enclave) and expose it via the ABI; broaden caveats beyond `max_bytes`.
2. **Real cross-node compute**: move the E2E demo transport from HTTP onto the composed mesh; resolve `ComputeTask.Component` CID → wasm bytes via IPLD; network promise pipelining.
3. **Runtime scale**: wasmtime Component Model + WASI P2 (richer than wazero), `gpu` capability dispatch (MLX / wgpu), zero-copy/RDMA data plane.
4. **Scheduler depth**: real telemetry-driven cost model in the live daemon; tensor sharding; multi-objective optimizer.
5. **Hardening**: mTLS + PeerID on the wire; persisted/rotating issuer key custody (Secure Enclave/TPM); rate limits/quotas enforced on the data plane.
6. **Ops**: real OTel tracing exported; metrics/health; **load tests** (many nodes, concurrent users) + **chaos tests** (partition, lid-drop, node loss) as automated suites; security audit/fuzzing.
7. **Frontier** (documented stubs): zk-WASM, host-TEE, RDMA-over-Thunderbolt.

## Where to pick up
Start a new session with: *"Read HANDOFF.md, CONTRACT.md, ARCHITECTURE.md. Continue Phase E item N (…)."* Pattern that has worked: pick one item, implement it for real, keep `go/cargo build+test` green + `go run ./test/e2e` passing, commit as hash066, push `main`. Keep the frozen contract frozen.
