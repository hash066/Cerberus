# Cerberus — Complete Chat Handoff (paste-into-next-chat context)

> **Purpose:** Everything a fresh chat needs to continue building Cerberus without re-deriving state.
> **Repo:** `github.com/hash066/Cerberus` · **Local:** `E:\10w10p\Cerberus` · **OS:** Windows 11 + PowerShell
> **Branches:** `integration` == `main` == `origin/main` == **`2c92204`** (all in sync as of this handoff)
> **Last verified:** 2026-07-01. CI run `28492497426` = **SUCCESS** (all hard gates green).

---

## 0. What Cerberus is
A **zero-trust distributed hypervisor for multi-agent orchestration**. Two machines form a mesh; you run sandboxed WASM workloads on a peer, share devices/files over a capability-secured 9P namespace, and expose an OpenAI-compatible gateway. Security model is **object-capability (ocap): capabilities, not identities — no ambient authority**; every cross-boundary call carries a signed capability handle.

- **Go** = control plane: `daemon/` (mesh, ninep, scheduler, telemetry, lifecycle, gateway, supervisor, api, metrics, auth, dataplane, dfs, devices), `cmd/` (`cerberusd`, `cerberus`). One module: `github.com/hash066/cerberus`.
- **Rust** = core: `core/` crates (`ocap`, `crdt`, `runtime`, `identity`, `economy`, `cabi`). One Cargo workspace at repo root.
- **Go ↔ Rust** meet only at `core/cabi` (C-ABI, opaque `u64` cap handles).
- **Host ↔ guest** meet only at the WIT world in `components/wit/`.
- **`tray/`** = Tauri v2 desktop app (the "Docker Desktop"-style GUI window).

**Canonical docs:** [ARCHITECTURE.md](ARCHITECTURE.md), [CONTRACT.md](CONTRACT.md), [CLAUDE.md](CLAUDE.md), [docs/workstreams.md](docs/workstreams.md), [docs/verticals/](docs/verticals/), [VISION-AND-ROADMAP.md](VISION-AND-ROADMAP.md), [LAUNCH-PLAN.md](LAUNCH-PLAN.md), [HANDOFF.md](HANDOFF.md).

---

## 1. Hard rules (from CLAUDE.md — these OVERRIDE defaults)
1. **Never edit the frozen contract casually:** `proto/`, `components/wit/`, `schemas/`, `contract/`. Changing them is a cross-lane event (see CONTRACT.md).
2. **Stay in your lane's dirs** (docs/workstreams.md + CONTRACT.md ownership).
3. **Stub what you consume** — every lane builds + unit-tests standalone against the frozen contract.
4. **Green or it didn't happen** — a change is done only when build + test pass.
5. **Capabilities, not identities** — no ambient authority anywhere.
6. **Maturity honesty** — do NOT fake stubs. Frontier pieces (zk-WASM proof-of-inference, RDMA-over-Thunderbolt, host-TEE memory shielding, MLX GPU, FUSE/WinFsp mounts, OS audio capture) are **documented stubs**, labeled as such. Never present them as working.

**Commit identity:** author/committer = `hash066 <harshitanagesh4@gmail.com>` → use `git commit --author="hash066 <harshitanagesh4@gmail.com>"`.

**Push policy:** user has EXPLICITLY authorized updating `main` ("update main to include all the proper commits"). `git push origin integration:main` is allowed. Keep `integration` and `main` in sync.

---

## 2. Green-bar commands (the definition of "done")
```powershell
# Go control plane
go build ./...
go vet ./...
go test ./...

# Rust core
cargo build --workspace
cargo test --workspace
cargo fmt --all -- --check
cargo clippy --workspace --all-targets -- -D warnings

# 2-node acceptance demo — MUST print 1337 ("signed cap verified")
go run ./test/e2e

# cgo real-Rust-kernel binding (Windows) — MUST pass 8/8
#   requires $env:ZIG set to zig.exe (see §4)
pwsh -NoProfile -File build/ffi.ps1 -Action test
```
`task build` / `task test` / `task lint` / `task demo` wrap these (Taskfile.yml). If `task` missing: `go install github.com/go-task/task/v3/cmd/task@latest`.

**CI** (`.github/workflows/ci.yml`) mirrors all of the above across ubuntu/macos/windows. **Hard gates:** go build/vet/test (3 OS), rust build/test (3 OS), rust fmt/clippy, e2e→1337, ffi-windows. **Advisory (`continue-on-error: true`, currently ✗):** `golangci-lint`, `cargo-audit` — do NOT block on them; promote to hard gates during v3 hardening.

---

## 3. Current state — what's DONE (all merged, green)
Phases **A–H core** + the **v3 "complete user flow"** wave are merged into `integration`/`main` at `2c92204`.

**Recent commits (newest first):**
```
2c92204 test(auth): make revocation-propagation test deterministic (fixes macOS CI)
375e469 Integrate v3-E: signed capability envelopes on the compute + data-plane wire (retire shared-kernel demo)
df38398 Integrate v3-C: Docker-Desktop-style tray dashboard (7 views + actions) (tray/)
85884f2 sec(mesh,dataplane,e2e): wire signed capability transfer onto the wire
9ad1465 Integrate v3-F: /cer/fs wired into the 9P namespace over the data plane (daemon/ninep, daemon/system)
72c9cd9 tray: v3 production desktop dashboard (see & control the mesh)
657bd9a Integrate v3-A: complete cerberus CLI (real run + nodes/devices/wallet/caps/conflicts/metrics) + DaemonRPC
ed105d2 feat(ninep,system): wire dfs into the 9P namespace so /cer/fs is real (G2 integration)
```

**v3 user-flow squad (6 lanes, all merged & green):**
| Lane | Delivered |
|---|---|
| **A — CLI** | `cerberus run <file.wasm> [--on peer]` (REAL dispatch, was a stub), `status`, `nodes`, `devices`, `wallet`, `caps mint\|attenuate\|revoke\|list`, `conflicts list\|resolve`, `metrics`, `version`, `--json`. Backing `DaemonRPC` in `cmd/cerberusd/rpc.go`; CLI types in `cmd/cerberus/rpctypes.go`. |
| **B — Gateway** | OpenAI-compatible `daemon/gateway/`: `/v1/chat/completions` (SSE streaming + non-stream), `/v1/models`, `/v1/completions`; `SetSettler`, `RegisterModel` seams. |
| **C — Desktop** | `tray/` Tauri v2 rebuilt into a **7-view sidebar dashboard** (Overview · Mesh · Devices · Workloads · Metrics · Wallet · Conflicts) + action buttons + `tray/preview.html` mock. `lib.rs` fetches `/api/v1/status`, `/metrics`, gateway with operator token. Builds to `tray/src-tauri/target/debug/tray.exe`. |
| **D — Docs** | README quickstart + `docs/getting-started.md`, `docs/cli.md`, `docs/gateway.md`, `docs/user-guide.md`, `docs/ffi.md`. |
| **E — Security** | **Signed caps on the wire** — retired the shared-kernel demo model. `daemon/auth/signedcap.go` + `keystore.go` wired into `daemon/mesh/compute.go` (`ServeComputeSigned`/`RequestComputeSigned`), `daemon/dataplane` (`SetSignedVerifier`/`RegisterSignedGrant`), and `test/e2e/node`. e2e still prints 1337. |
| **F — Storage** | `daemon/dfs/dfs.go` (1 MiB chunks, SHA-256 CIDs, Reed-Solomon k=4/m=2) wired via `daemon/ninep/fs.go` + `daemon/system/fs.go` into **`/cer/fs`** (write→read over the data plane, cap-checked). |

**Real Rust kernel over cgo (Phase E item 1) — DONE:**
- `core/cabi/src/lib.rs` + `core/cabi/include/cerberus.h`: `cerberus_cap_mint/attenuate/verify/revoke/is_revoked/issuer/backend/version` over `SignedKernel`; plus `cerberus_blockstore_put/get_len/get/has` and `cerberus_revoke/is_revoked/revocations_merge/export_len/export` (runtime BlockStore + crdt RevocationSet).
- `daemon/ffi/kernel_ffi.go` + `engines_ffi.go` (`//go:build ffi`) map `contract.CapKernel` onto the ABI. `Backend()` = `"rust-signed-cabi"`.
- Build recipe in `build/ffi.ps1` + `docs/ffi.md`. **8/8 ffi tests pass** locally and in CI (`ffi cgo kernel (windows)`).

**Daemon composition (`cmd/cerberusd/main.go`):** bind-address flags (`-gateway-addr -api-addr -metrics-addr -rpc-addr`); wired Fabric, Scheduler, devices, ledger, crdtEngine into `DaemonRPC`; metrics/health server on `127.0.0.1:7779` (`/metrics` token-gated, `/healthz`, `/readyz`); api `/api/v1/status`.

---

## 4. The cgo toolchain gotcha (critical, non-obvious)
**cgo on Windows cannot drive MSVC `cl.exe`** — it needs a GCC-compatible compiler. The working recipe (encoded in `build/ffi.ps1`, documented in `docs/ffi.md`):
- **zig cc** as the C compiler/linker + Rust **`x86_64-pc-windows-gnu`** staticlib + `GOARCH=amd64`.
- Build the staticlib specifically (the crate also builds a cdylib that needs mingw `dlltool`, which is absent):
  `cargo rustc -p cerberus-cabi --target x86_64-pc-windows-gnu --crate-type staticlib`
- **`__chkstk_ms` duplicate-symbol** collision (zig `compiler_rt` vs Rust `compiler_builtins`): strip the lone member from the archive via `llvm-nm --print-armap` + `llvm-ar d`, then link `-lunwind` + Windows syslibs (`-lbcrypt -lntdll -luserenv -lws2_32 -ladvapi32 -lkernel32`).
- **wasmtime made the ffi build fail** (pulls windows-sys→mingw dlltool). Fix: `wasmtime` is an **optional default-on feature** in `core/runtime`; `core/cabi` depends on runtime with `default-features = false`. `gpu` (wgpu) feature is **default-OFF**.
- Go LDFLAGS in `daemon/ffi/kernel_ffi.go`:
  `#cgo windows LDFLAGS: -L${SRCDIR}/../../target/x86_64-pc-windows-gnu/debug -lcerberus_cabi -lunwind -lbcrypt -lntdll -luserenv -lws2_32 -ladvapi32 -lkernel32`

**zig location (this machine):**
`C:\Users\asus\AppData\Local\Temp\claude\E--10w10p-Cerberus\d4531571-4594-46ef-ba90-75e3eca613d9\scratchpad\tools\zig-windows-x86_64-0.13.0\zig.exe`
Set before running the ffi build: `$env:ZIG = "<that path>"`. (In CI, `mlugg/setup-zig@v1` + a step that writes `ZIG=$((Get-Command zig).Source)` to `$GITHUB_ENV`.)

---

## 5. How to run it (manual test)
```powershell
# 1. Build (if needed)
go build -o bin/cerberusd.exe ./cmd/cerberusd
go build -o bin/cerberus.exe  ./cmd/cerberus

# 2. Start the daemon (headless engine)
E:\10w10p\Cerberus\bin\cerberusd.exe   # writes operator token to %APPDATA%\cerberus\operator.token

# 3. Talk to it via CLI (new shell)
$env:CERBERUS_TOKEN = Get-Content "$env:APPDATA\cerberus\operator.token"
E:\10w10p\Cerberus\bin\cerberus.exe status
E:\10w10p\Cerberus\bin\cerberus.exe nodes
E:\10w10p\Cerberus\bin\cerberus.exe devices
E:\10w10p\Cerberus\bin\cerberus.exe wallet
E:\10w10p\Cerberus\bin\cerberus.exe caps mint --kind vram --rights read,alloc

# 4. The DESKTOP APP (the Docker-Desktop-style GUI window) — needs cerberusd running + WebView2 (present on Win11)
E:\10w10p\Cerberus\tray\src-tauri\target\debug\tray.exe
#   (build it if missing: cargo build --manifest-path tray/src-tauri/Cargo.toml)
```
**Binaries present now:** `bin/cerberusd.exe`, `bin/cerberus.exe`, `tray/src-tauri/target/debug/tray.exe`.
**NOTE:** the daemon/tray processes from the previous session are **no longer running** — relaunch them to test.

Dashboard panels **Overview/Mesh/Metrics/Wallet** are live; **Devices/Workloads/Conflicts** show an honest "pending daemon support" where the HTTP route isn't wired yet (see §6).

---

## 6. What's NOT done yet — the actionable backlog

### 6a. Lead composition wiring (agents left clean seams; just needs connecting)
These are the highest-value next tasks — pure integration, no new subsystems:
- Wire `gateway.SetSettler` → `economy.Settler` (real settlement on completions).
- Wire `api.NewWithGetters(issuer, Getters{...})` for the rich status snapshot (devices, wallet, belief_conflicts, metrics, subsystems).
- Register `/v1/models` output from the actual model registry.
- Wire the daemon HTTP routes the tray marks **`PENDING_DAEMON`**: devices list, conflicts list/resolve, cap-revoke over HTTP, workload history. (This makes the Devices/Workloads/Conflicts dashboard panels live.)
- Feed metric counters at their real call sites: scheduler (`TasksPlaced`), dataplane (`TransfersTotal`/`BytesTransferred`), gateway, auth (`RevocationsTotal`), mesh (`Peers`).
- `system.Compose` does **not** call `ServeCompute` → the remote worker-side compute handler is unregistered. Wire it so a peer actually accepts dispatched workloads end-to-end (beyond the e2e harness).

### 6b. Hardware / human gates (cannot be done headlessly by an agent)
- Real multi-machine bring-up (two physical nodes on a LAN).
- FUSE/WinFsp mounts for `/cer/fs` (currently in-process namespace, not an OS mount).
- OS audio capture / real speaker+mic sharing (9P peripheral virtualization is scaffolded, not capturing real hardware).
- MLX / GPU compute backend (wgpu feature exists but off; no real GPU dispatch).
- TEE custody, canonical CBOR cap wire form (`schemas/capability.cddl` — a **contract change**, needs the cross-lane process), Zenoh intra-site transport, zk-WASM proof-of-inference, RDMA-over-Thunderbolt.

### 6c. CI hardening
- Promote `golangci-lint` and `cargo-audit` from advisory (`continue-on-error: true`) to hard gates once their findings are cleaned up. `cargo-audit` currently flags a RustSec advisory in a transitive dep (libp2p/wasmtime/wgpu tree); `golangci-lint` reports lint issues. Both are non-blocking today by design.
- Minor: `actions/checkout@v4` / `setup-go@v5` emit a Node 20-deprecation annotation (cosmetic).

---

## 7. Parallel-agent workflow (how the big waves were run)
- Spawn agents with the `Agent` tool: `isolation: "worktree"` + `run_in_background: true`, **disjoint directory ownership**, each builds standalone against the frozen contract.
- Lead merges each worktree with `git merge --no-ff` (clean because dirs are disjoint), then reconciles `go.mod` / `Cargo.lock` and re-runs the full green bar (§2) before pushing.
- GitHub Actions gotcha learned the hard way: **expressions are NOT allowed in a `uses:` action ref** (`uses: foo/bar@${{ ... }}`) → the run dies at 0s with a "workflow file issue". Fixed by hard-coding `uses: dtolnay/rust-toolchain@stable`.

---

## 8. Known-flaky test that was fixed (don't regress it)
`daemon/auth/gossip_test.go :: TestRevocationPropagatesAcrossNodes` flaked on macOS CI only — a publish-vs-Subscribe race on the in-process stub fabric (one-shot gossip dropped before B's subscription registered). **Fix:** re-issue the revoke in a bounded 3s loop until B applies it (idempotent; models OR-set re-sync on reconnect). Verified green 10×. If you touch the gossip/revocation path, keep this loop — don't revert to a single one-shot revoke.

---

## 9. Environment quick-reference
- **Working dir:** `E:\10w10p\Cerberus` · **Shell:** PowerShell 7+ (pwsh); Bash tool also available.
- **Go module:** `github.com/hash066/cerberus`. **Cargo workspace:** repo root (tray excluded via its own `[workspace]`).
- **Ports:** metrics/health `127.0.0.1:7779` (7778 was taken by an unrelated PID). Gateway/api/rpc have `-*-addr` flags.
- **Operator token:** `%APPDATA%\cerberus\operator.token`; pass as `$env:CERBERUS_TOKEN` or `Authorization: Bearer`.
- **gh CLI** is authed (used for CI inspection: `gh run list`, `gh run view <id> --log-failed`).

---

## 10. Suggested first move in the next chat
CI is green and everything is pushed, so there's no firefighting. The highest-leverage next work is **§6a (composition wiring)** — start by wiring the `PENDING_DAEMON` HTTP routes (devices, conflicts list/resolve, cap-revoke, workload history) so the last three dashboard panels go live, then feed the metric counters at their call sites. That turns the "pending daemon support" placeholders into real data end-to-end. Keep each change behind the green bar (§2) and push `integration` → `main` in sync.
