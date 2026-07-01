# Cerberus — Session Handoff / Compact Context

> Read this first in a new session. It is the single source of "where things actually stand" — what is real, what is stubbed, what is blocked, and exactly how to continue. Honest by design.

---

## ⚠️ SUPERSEDING UPDATE — 2026-07-01

Everything below this section (the original doc) was written before the "v3" integration wave (signed capability envelopes on the wire, tray dashboard rewrite, `/cer/fs` wired into 9P) and before the segment described here. Where the two disagree, **trust this section**. The old content below is still useful for deep historical/phase context (E/F/G/H roadmap reasoning) — just don't treat its "Reality matrix" or "Phase E roadmap" tables as current without cross-checking against this update.

### Current repo state (verified 2026-07-01)
- Branch `integration`, HEAD `fa0e9fb`. `main` is kept in sync (fast-forwarded to the same HEAD). **CI on `fa0e9fb` is green** (run conclusion = success). See the "CI restored to green" subsection below for what was fixed — `c220fa4` (the previous HEAD) was actually **red**, contrary to the pre-fix assumption in this doc.
- Recent commit chain (newest first): `c220fa4` merge → `c2b96fa` tray HTTP routes + audio registration → `81857a1` mesh capability-gating → `9c14bc5` wazero sandboxing → `43e94e4` economy Challenge wiring → `9ae09f5` tray Docker-Desktop UI rewrite → `5fe7037` AcquireLock TOCTOU fix. Further back: `375e469` signed capability envelopes on compute+dataplane wire (retired the old shared-kernel demo model), `df38398` first tray dashboard integration, `9ad1465` `/cer/fs` wired into the 9P namespace over the data plane.
- Harmless stale worktree dirs exist under `.claude/worktrees/agent-*` (older, unrelated agent runs from Jun 30–Jul 1 morning, branches named `worktree-agent-*`). Not blocking anything; clean up opportunistically with `git worktree remove`.

### What the most recent segment did (all merged, tested, pushed, live-verified)
Triggered by: an adversarial "Principal/Staff engineer" PR-review audit (assume nothing works, verify everything) → a second stub audit → a combined ask to (a) match a Docker-Desktop UI layout screenshot in the tray app, (b) support light+dark mode, (c) fix every issue from both audits, (d) add an audio-devices sidebar page.

| Fix | Key files | How it was verified |
|---|---|---|
| **P0**: mesh shard/component RPCs had zero auth (any LAN peer could read/write shards) | `daemon/mesh/shard.go`, `daemon/mesh/component.go`, `daemon/system/shardstore.go` — self-issued stream-bound signed-capability scheme (issuer must equal the QUIC/TLS-authenticated remote PeerID for that stream) | `daemon/mesh/shard_test.go`: fail-closed tests proving denial happens before the store is touched, concurrent fan-out tests |
| **P0**: the actual *live* WASM execution path (wazero — `core/runtime`'s wasmi/wasmtime is dead/unreachable code, `core/cabi` exposes no exec function) had zero resource governance | `daemon/wasm/wasm.go` — `governedRuntimeConfig()`, `withBoundedDeadline()`, `DefaultTimeout=5s`, `DefaultMemoryLimitPages=64`, real `WithCloseOnContextDone` cooperative preemption | `daemon/wasm/wasm_test.go`: hand-encoded infinite-loop / memory-hog raw wasm modules, proven bounded not hung |
| `AcquireLock` TOCTOU race in the single-instance daemon lock | `daemon/discovery/manifest.go` — atomic `O_CREATE\|O_EXCL` retry loop | `TestAcquireLockIsRaceFree`: 64 concurrent goroutines, exactly 1 wins, 63 get `ErrAlreadyRunning` |
| Fraud-proof Challenge/Slash mechanism was unreachable (no RPC/CLI surface) | `cmd/cerberusd/rpc.go` (`DaemonRPC.EconomyChallenge`), `cmd/cerberusd/main.go`, `cmd/cerberus/main.go` (`cerberus economy challenge <tx-id> --component-cid --input-cid --claimed-output-cid --actual-output-cid [--challenger]`) | Unit tests + live CLI smoke test (`cerberus help | grep economy`) |
| Tray's 5 remaining `PENDING_DAEMON` panels had no backing routes | `daemon/api/routes.go` (new): `GET /api/v1/conflicts`, `POST /conflicts/resolve`, `GET /devices`, `GET /workloads`, `POST /cap/revoke`, `POST /devices/grant`; `daemon/api/api.go` extended with `Actions`/`ListGetters` | Live `curl -H "Authorization: Bearer $TOKEN"` against a running daemon — real JSON, not 404 |
| No audio device *discoverability* existed | `daemon/audio/audio.go` (`EndpointInfo`/`EndpointKind`), `daemon/audio/os_windows.go` (`EnumerateEndpoints()` — real WASAPI `IMMDeviceEnumerator.EnumAudioEndpoints`, reads `PKEY_Device_FriendlyName`), `daemon/audio/os_other.go` (honest empty stub, non-Windows), `daemon/system/system.go` (`registerAudioDevices`) | Live: found 1 real mic + 1 real speaker on this machine, registered at `/cer/dev/audio/{mic,speaker}/0`, showed up in `cerberus devices` |
| Tray UI didn't match the requested Docker-Desktop layout; no theming | `tray/src/index.html`, `tray/src/styles.css`, `tray/src/main.js` — full rewrites (done directly, not via agent): global search bar w/ Ctrl+K, settings gear → modal with light/dark/system theme pills (`localStorage` key `cerberus-theme`), checkbox+name+columns+actions table layout, new "Audio devices" sidebar page | Verified live via browser-preview tools: computed CSS values inspected under both themes, settings panel opened/closed, theme toggle clicked, search filter tested |

**Note on the audio row above**: this only adds *device enumeration* (discovering that a mic/speaker exists and exposing it at `/cer/dev/audio/...`). Actual OS audio *capture/playback* wiring (`daemon/audio`'s packetized transport riding `daemon/audiolink`) was already real from an earlier phase (see the old Reality Matrix below) — this segment did NOT touch that, only discovery/registration.

### Full verification performed on `c220fa4` (all green)
- `go build ./...`, `go vet ./...`, `go test ./...` — 30 packages, all pass.
- `go run ./test/e2e` → `DEMO PASSED: hello-shard.wasm ran remotely on worker and returned 1337.`
- Cross-compile (proxy for CI's ubuntu/macos legs): `GOOS=linux GOARCH=amd64 go vet ./...` and `GOOS=darwin GOARCH=arm64 go vet ./...` — both clean.
- `cargo build --manifest-path tray/src-tauri/Cargo.toml` — clean.
- Rebuilt all 3 Go binaries fresh (`bin/cerberusd.exe`, `bin/cerberus.exe`, `bin/cerberus-mcp.exe`) and ran a live manual smoke test against a real running daemon: `cerberus doctor` (all ok), `cerberus devices` (shows real audio hardware + vram), `cerberus caps mint`, direct `curl` against `/api/v1/devices`/`conflicts`/`workloads`.
- Pushed both `origin/integration` and `origin/main` to `c220fa4`.

### CI restored to green (2026-07-01, after `c220fa4`)

The "was CI actually green on `c220fa4`?" check that this doc flagged as not-yet-done was run — and `c220fa4` was **red**. Fixing the first failure (a Windows build break) unmasked a cascade of latent failures that had never actually run in CI. All are now fixed across `b99e3a9` → `71f62d9` → `fa0e9fb`; the run on `fa0e9fb` is green.

| Failure (job) | Root cause | Fix |
|---|---|---|
| Windows `go build ./...` | hosted `windows-latest` ships gcc, so `CGO_ENABLED` defaulted **on**, pulling cgofuse's cgo variant (`#include <fuse_common.h>`) instead of the no-cgo WinFsp DLL-loader `daemon/ninep/mount_windows.go` targets | pin `CGO_ENABLED=0` on the `go` job in `.github/workflows/ci.yml` (matches the sanctioned pure-Go default build; the real cgo path stays covered by `ffi-windows`) |
| Linux `daemon/discovery` `TestAcquireLockIsRaceFree` (2 winners) | **real bug**: `AcquireLock`'s `O_CREATE\|O_EXCL` created an empty file then wrote the PID separately; a racer read the empty file, deemed it stale, and unlinked it (POSIX allows unlinking an open file) → two owners. Windows masked it. | `daemon/discovery/manifest.go`: write PID to a temp file, then `os.Link` it into place (lock path always carries a valid PID the instant it exists) |
| macOS `cmd/cerberus-mcp` `TestResolveAddrs_*` (`19092` vs default) | test config-dir isolation set only `%AppData%`/`$XDG_CONFIG_HOME`, but macOS `os.UserConfigDir()` uses `$HOME/Library/Application Support` → tests shared the real dir; `cmd/cerberus` `discovery_test` wrote a manifest `cmd/cerberus-mcp` then read | add `HOME` to the isolation helpers in `daemon/discovery`, `cmd/cerberus`, `cmd/cerberus-mcp` test files |
| macOS `test/chaos` `TestRealProcessKillAndRestartConvergesRevocation` (token timeout, then mDNS) | (1) test read the child daemon's token at `configDir/cerberus/...` but on macOS the child writes under `configDir/Library/Application Support/cerberus/...`; (2) GitHub macOS runners restrict the multicast libp2p mDNS needs, so two daemons never converge | (1) `nodeConfigDir()` helper reads the OS-correct path; (2) skip the test only under `GOOS=darwin && GITHUB_ACTIONS` (real-mDNS path still covered on Linux/Windows and on a real Mac) |
| Windows `daemon/mesh` `TestConcurrentPublishSubscribeNoCorruption` (90/125) | test asserted lossless delivery, but `dispatch()` intentionally drops on a full 64-buffer ("drop for slow consumers rather than block the bus") → exact count is scheduler-dependent | assert the real lossy-bus invariants instead (own key only / stays registered / never over-delivers) |
| Windows `daemon/audio` + `daemon/system` audio tests | asserted a real WASAPI render endpoint exists; headless CI runners have none | skip on zero devices (enumeration succeeding is the real bar); hardware assertions still run on a dev box |

**Still red, but non-blocking (`continue-on-error: true`, unchanged by this segment — they were red on the last known-good run too):**
- `golangci-lint`: the pinned linter was built with go1.24 but `go.mod` targets `go 1.25.7`, so it refuses to run (`can't load config: ... lower than the targeted Go version`). Fix = bump `golangci/golangci-lint-action`/linter to a go1.25-built release.
- `cargo-audit`: 18 RustSec advisories, all in `wasmtime 37.0.3` / `wasmtime-wasi` (incl. one critical Winch sandbox-escape). Fix = upgrade wasmtime; note the full set needs `wasmtime-wasi >= 46.0.1` (a 37→46 bump, an API-breaking "v3 hardening" task, not a CI-green fix).

These two are the intentionally-advisory gates ci.yml marks "promote to a hard gate as part of v3 hardening" — pick them up there.


### Standing rules for this project (apply automatically, do not re-ask)
1. **Commit attribution**: every commit uses `git commit --author="hash066 <harshitanagesh4@gmail.com>"`, **no** `Co-Authored-By` trailer, no AI/Claude attribution anywhere in the message.
2. **Keep `main` and `integration` in sync** — push to both after merging (standing authorization already given, no need to re-ask).
3. Follow [CLAUDE.md](CLAUDE.md)'s golden rules: never edit frozen contract dirs (`proto/`, `components/wit/`, `schemas/`, `contract/`) unilaterally; stay in lane; stub what you consume; green (`task build`/`task test`) before calling something done.
4. Parallelizable work has been done via **git-worktree-isolated background agents** (`Agent` tool, `isolation: "worktree"`, `run_in_background: true`). Agents do **not** auto-commit — after each finishes: commit inside its worktree, `git merge --no-ff --no-edit` from the main checkout, resolve conflicts, then move to the next.
5. Maturity honesty (ARCHITECTURE.md §8): zk-WASM proof-of-inference, RDMA-over-Thunderbolt, host-TEE memory shielding are **documented stubs** for v0.1 — never fake these as working.

### MCP / OS-discoverability (from earlier in this project, before this segment)
The user wants Cerberus usable as an MCP server from Claude Code/Cursor plus general OS-level discoverability. Existing (unverified-live in this exact session, confirm before assuming still correct):
- `cmd/cerberus-mcp` — MCP server binary (`github.com/modelcontextprotocol/go-sdk v1.6.1`).
- `.mcp.json` / `.cursor/mcp.json` for auto-discovery.
- `daemon/discovery` — connection-manifest (`daemon.json` in `%AppData%\cerberus\`) + atomic single-instance lock (the TOCTOU fix above hardened this).
- WinFsp/cgofuse integration code for `/cer/fs` FUSE mounting was written but **could not be installed/verified live** (no admin rights in this session) — treat as unverified until someone with admin can install WinFsp and smoke test.

### Known gaps, explicitly deprioritized (don't re-litigate, pick up only if asked)
- **GPU compute dispatch not wired into the scheduler** — `core/runtime/src/gpu.rs` has a real wgpu backend (verified to run on this box's NVIDIA RTX 3050 per an earlier phase) but it's orphaned/unreachable from the scheduler. Swapped out this segment in favor of the higher-priority wazero sandboxing fix. **Most likely next real feature.**
- **Real Zenoh (zenoh-c)** — still stubbed, not touched this whole project.
- **zk-WASM, host-TEE, RDMA-over-Thunderbolt** — intentionally-honest Frontier stubs per ARCHITECTURE.md §8. Not a bug.
- Stale worktree dirs mentioned above — harmless disk cleanup.

### If the user's next message is just "continue" or similar
1. CI is already green on `fa0e9fb` (`gh run list --limit 4 --branch integration` to re-confirm) — the CI-fix segment above is closed out. Nothing is implicitly queued except the deprioritized items below and the two non-blocking advisory gates (golangci-lint go-version, wasmtime cargo-audit).
2. If there's no new direction, ask what's next rather than guessing — the most likely real feature is **GPU compute dispatch into the scheduler** (see "Known gaps"); the natural CI-hardening follow-up is promoting golangci-lint + cargo-audit to hard gates (fix the linter go-version pin and the wasmtime upgrade).

---

## Historical context below (pre-v3-wave — see superseding update above for current truth)

## What Cerberus is
A zero-trust **distributed hypervisor for multi-agent orchestration**: bind heterogeneous machines (Macs/PCs/Linux) into a local, capability-secured mesh that runs autonomous agent swarms — escaping centralized cloud. **Desktop app** (headless daemon `cerberusd` + CLI `cerberus` + Tauri tray UI). Go = control plane; Rust = capability kernel / WASM / crypto / CRDT. Full vision in [ideadumpp2.md](docs/research/ideadumpp2.md); canonical spec in [ARCHITECTURE.md](ARCHITECTURE.md); per-vertical designs in [docs/verticals/](docs/verticals/).

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
| Composed daemon (OCap + mesh + telemetry + scheduler + 9P under OTP supervisor) (`daemon/system`) | ✅ REAL (composition); mesh = real libp2p/QUIC. **Cross-cut wiring done:** the 9P namespace is bridged to the QUIC data plane (`ninep.Server.SetGranter`) — opening `.../ctl` calls `dataplane.RegisterGrant` and returns the live endpoint; the 9P wire server + data-plane receiver both run under the supervisor (addrs in `System.NinePAddr`/`DataPlaneAddr`). Proven end-to-end by `daemon/ninep` `TestOpenCtlGrantsRealDataPlaneTransfer` (open ctl → real QUIC transfer within quota, rejected past it). |
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
| Network audio transport — mic/speaker (`daemon/audio`) | ✅ REAL, tested (Phase F5) — packetized sender/receiver, reordering jitter buffer, DLL drift control, gap-fill. **Now rides the data plane:** `daemon/audiolink` carries an audio session as one capability/quota-bound QUIC transfer (packets length-framed by `audio.SendTransport`/`RecvTransport`); `audio` stays a leaf (depends only on its `Transport` interface). Proven end-to-end by `audiolink` `TestAudioRidesDataPlane` (SineSource → QUIC under a grant → reconstructed). OS capture (CoreAudio/WASAPI/PipeWire) = labelled stub. |
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
  - **Cross-cut wiring** ✅ (`daemon/ninep`,`daemon/system`,`cmd/cerberusd`): 9P `ctl` open → `dataplane.RegisterGrant` → returns the real endpoint (ARCHITECTURE §4.1); the 9P2000.L wire server and the QUIC data-plane receiver are both supervised in `system.Compose` and their addresses logged at startup. `ninep.Server` gained an injectable `Granter` so the namespace stays decoupled from the data plane (no import cycle); falls back to the descriptor-only placeholder when unwired. Tested by `TestOpenCtlGrantsRealDataPlaneTransfer`.
  - **Audio over the data plane** ✅ (`daemon/audiolink`, `daemon/audio`): an audio session rides one capability/quota-bound QUIC transfer; `audio.SendTransport`/`RecvTransport` length-frame packets onto the byte stream, `audio` stays a leaf, `audiolink` is the composition seam (audio ↔ dataplane). Tested end-to-end + race-clean.
  - **Next:** Phase F2 (GPU dispatch) on real hardware; full mTLS/PeerID-pin on the data-plane transport; 9P `ctl`/audio activation in the live daemon once an OS capture source exists.
- **Phase G + H (launch wave) — landed via 8 parallel agents** (isolated worktrees, integrated on `integration`, all green + e2e + ffi; baseline `15a15d8`). See [LAUNCH-PLAN.md](LAUNCH-PLAN.md).
  - **G1** ✅ Causal CRDT merge (vector clocks + multi-value registers) — concurrent `agent.belief` contradictions surface as `BeliefConflict`s (never silent LWW) with durable human-`Resolve` (`core/crdt`, `daemon/state`). *Note: state on-disk doc format changed — fresh stores only.*
  - **G2** ✅ Distributed FS `/cer/fs` — 1 MiB chunking + SHA-256 CIDs + Reed-Solomon (k=4/m=2), survives any 2 shard losses, integrity-checked (`daemon/dfs`).
  - **G3** ✅ eUTXO optimistic settlement + fraud-proof slashing, durable + restart-safe (`core/economy`, `daemon/ledger`, `daemon/economy`); zk-WASM = stub.
  - **Sec** ✅ Signed capability envelope on the wire — a node can trust a cap it didn't mint (Ed25519, monotone narrowing, key rotation, TEE custody hook) (`daemon/auth`). *Closes the "shared-kernel demo model" stub; API ready, mesh/dataplane wiring is the remaining lead step.*
  - **Obs** ✅ Metrics/health + trace-tree (`daemon/metrics`, `daemon/telemetry`) — **wired into `cerberusd`**: `/metrics` (token-gated), `/healthz`, `/readyz` on `127.0.0.1:7779`; peer gauge live. Verified at runtime.
  - **Chaos** ✅ Multi-node simulation suites (`test/chaos`, `test/load`) — partition→heal convergence, node-loss reroute, lid-drop checkpoint+promote, placement/revocation storms with leak assertions. (First validation beyond a single box.)
  - **Release** ✅ CI pipeline (`.github/workflows`: build/test/fmt/clippy/e2e + golangci-lint + cargo-audit on an OS matrix + a Windows ffi job) + cross-platform packaging (`build/release.{sh,ps1}`); signing/notarization scaffolded honestly.
  - **Runtime** ✅ Real **WASI-P2** component (cargo-component fixture → 1337 through `wasmtime-wasi`) + **GPU dispatch** abstraction: software backend tested; real **wgpu** backend (default-OFF `gpu` feature) **ran on this box's NVIDIA RTX 3050** (`core/runtime`). cabi stays wasmtime/wgpu-free (ffi preserved).
  - **Remaining lead cross-wires (each its own focused, verified pass):** signed caps → `mesh/compute.go` + `dataplane` (retire the opaque-u64 transfer; touches the e2e acceptance path); `/cer/fs` (`daemon/dfs`) → the 9P namespace; settlement (`economy.Settler`) → gateway compute-completion; CRDT conflicts/resolve → status API/tray; metric counters fed at scheduler/dataplane/gateway/auth call sites.

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
