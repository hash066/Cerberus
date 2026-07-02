# RESUME — pick up the beta feature-fix work (context handoff)

> Written mid-task when the previous chat hit its context limit. This is the single
> source of "exactly where things stand and what to do next." Also read
> [HANDOFF.md](HANDOFF.md) (longer history) and [CLAUDE.md](CLAUDE.md) (rules).
> Date: 2026-07-02.

## 0. Immediate state (verified)

- Branch `integration`, HEAD **`fb7a21e`**. `main` is synced to the same commit. CI is **green** (only the advisory `golangci-lint` job is red — intentional, `continue-on-error`).
- **UNCOMMITTED, UNVERIFIED work in the tree right now** (the #7 fix — see §2): `daemon/wasm/wasm.go`, `daemon/gateway/chat.go`, `daemon/gateway/completions.go`. These compile-looking edits were made but the previous chat was interrupted **before** rebuilding/testing them. **Verify → commit, or `git checkout --` to discard, before doing anything else.**
- A `cerberusd` may be running locally (built to `bin/cerberusd.exe`, gitignored). `bin/` and `tray/src-tauri/binaries/` and `.claude/` are gitignored.

## 1. Standing rules (do not re-ask)
- Commit as **`git commit --author="hash066 <harshitanagesh4@gmail.com>"`** — NO Co-Authored-By, no AI attribution.
- Keep `main` and `integration` in sync: after committing to `integration`, `git checkout main && git merge --ff-only integration && git push origin main && git checkout integration`. A settings.local permission rule already allows `git push origin main` without prompting.
- Green before done: `CGO_ENABLED=0 go build ./... && go test ./...`; `go run ./test/e2e` → `1337`. CI hard gates: go (3 OS), `go test -race` (ubuntu), `fuzz smoke`, `cargo-audit`, rust, e2e, ffi, SBOM.
- Frozen contract dirs off-limits (`proto/`, `components/wit/`, `schemas/`, `contract/`).
- Phase 1 security lanes (DoS/quota, threat model, golangci-lint burn-down) are **PAUSED** per the user; current priority = beta feature-completeness + the two questions below.

## 2. THE CURRENT TASK: fix feature-audit items #7, #8, #10, #11

Plain-language audit context: from a double-click install the app can install, **auto-start its bundled daemon** (one-click sidecar — DONE, verified), show node status, see machines join (multi-machine — DONE, proven), see devices, metrics. The broken/partial things the user asked to fix:

### #7 — "Run a workload" returns empty  → FIX IN PROGRESS (uncommitted, UNVERIFIED)
Root cause found: the gateway executor calls WASM entry **`"run"`**, but the `hello-shard` fixture exports **`"hello_shard"`** (`test/e2e/node/node.go:51` `HelloShardExport = "hello_shard"`). So exec failed with `no exported function "run"`, and the gateway handler **swallowed the failure** (used `result.Output` without checking `result.OK`) → HTTP 200 with empty content.

Edits already made (uncommitted):
- `daemon/wasm/wasm.go`: added `RunI32Any(ctx, module, entries)` (instantiates once, tries candidate entries); added `var extraEntries = []string{"hello_shard","_start","main"}`; `Executor.Dispatch` now calls `RunI32Any(ctx, module, append([]string{e.entry}, extraEntries...))`.
- `daemon/gateway/chat.go` and `completions.go`: after `dispatch`, if `!result.OK` return a real error (HTTP 502 `workload failed: <err>`) instead of empty 200.

**NEXT STEP (do this first):** rebuild + verify:
```
CGO_ENABLED=0 go build ./... && go test ./daemon/wasm/... ./daemon/gateway/... -count=1
# then run the daemon and hit the gateway:
CGO_ENABLED=0 go build -o bin/cerberusd.exe ./cmd/cerberusd
bin/cerberusd.exe &            # writes token to %APPDATA%\cerberus\operator.token
TOK=$(cat "$APPDATA/cerberus/operator.token")
curl -s -H "Authorization: Bearer $TOK" -H "Content-Type: application/json" \
  -X POST http://127.0.0.1:8080/v1/chat/completions \
  -d '{"model":"hello-shard","messages":[{"role":"user","content":"hi"}]}'
# EXPECT: choices[0].message.content == "1337"
```
If it returns `1337`, #7 is fixed — commit it. (Verification via PowerShell was the step interrupted.)

### #8 — Run history always empty  → LIKELY FIXED BY #7, verify
The plumbing already exists: `gateway.SetOnDispatch` is wired in `cmd/cerberusd/main.go` (~line 319) to a workload log that the `GET /api/v1/workloads` route reads. It was empty only because every dispatch was FAILING (see #7). After #7, a successful run should append an entry.
**NEXT:** after a successful run, `curl -H "Authorization: Bearer $TOK" http://127.0.0.1:7777/api/v1/workloads` and confirm the run shows up. If the hook only records on success, confirm failures are also visible or intentionally omitted. If nothing records, inspect the `SetOnDispatch` callback + the `/workloads` handler in `daemon/api/routes.go`.

### #10 — Wallet: balance shows, but no transactions / spend  → NOT STARTED
- Real pieces: durable eUTXO ledger `daemon/ledger`; the gateway has a settlement seam — `daemon/gateway/settlement.go` (`recordSettlement`, `SetSettler`, `PricingPolicy`); status API returns `operator_balance` + `wallet.balance` (works).
- **What to do:** decide scope (beta = credits-only, no real value transfer). Minimum honest win: surface the real ledger as a **transactions list** in the wallet, and make a workload run reflect a settlement entry. Check whether `cmd/cerberusd` wires a real `Settler` + `PricingPolicy` into the gateway (if not, wire the ledger-backed one), and whether there's a `/api/v1/wallet` or transactions route (add one to `daemon/api/routes.go` + a `wallet()`/`transactions()` command in `tray/src-tauri/src/lib.rs`, which currently only reads balance from status). Keep value-transfer OFF for beta; be honest in the UI.

### #11 — Belief-conflicts: panel + resolve exist, none flow through  → NOT STARTED
- Real pieces: causal-CRDT belief-conflict detection in `daemon/state` (concurrent contradictory `agent.belief` writes surface as `BeliefConflict`, durable human `Resolve`); routes `GET /api/v1/conflicts` + `POST /api/v1/conflicts/resolve` exist and return `[]` (no conflicts present in normal solo use).
- **What to do:** prove detect→list→resolve end-to-end. Likely need a way to actually CREATE a conflict (two nodes / two concurrent writes to the same belief key) so one appears in `/conflicts`, then resolve it via the route and confirm it clears. Verify the route is wired to the live `daemon/state` engine (not a stub) in `cmd/cerberusd` + `daemon/api`. If detection only happens cross-node, use the two-daemon setup (see §4).

## 3. THE TWO USER QUESTIONS (still unanswered — owe the user a reply)

### Q1: "Can devices actually SHARE vram/gpu/ram/storage/mic/speakers across machines on the same network?"
What's known so far (NEEDS confirmation before answering the user):
- Devices are **registered + discoverable**: `daemon/system` registers a static VRAM device + real audio endpoints (WASAPI mic/speaker) into the 9P namespace; `GET /api/v1/devices` returns them (verified: vram, mic, speaker).
- The **transport for actually using a remote device** is the capability-gated 9P namespace (`daemon/ninep`) → opening `.../ctl` returns a **data-plane endpoint** (`daemon/dataplane`, QUIC, now mTLS+PeerID-pinned) for bulk bytes; audio rides it via `daemon/audiolink`.
- **OPEN QUESTION to verify:** is cross-node device USE actually wired end-to-end (node B opening node A's `/cer/dev/...` over the mesh and streaming bytes), or is it registration + single-box only? GPU compute dispatch is known-orphaned (wgpu backend not wired into the scheduler — see HANDOFF "Known gaps"). Storage = `daemon/dfs` (Reed-Solomon distributed FS) — check if exposed to users. **Investigate `daemon/ninep` wire + `daemon/system` cross-node open + `daemon/dfs` before giving the user a per-device Yes/No.** Honest expected answer: audio transport is real; VRAM/GPU is registered but compute-dispatch is orphaned; storage (dfs) exists but check user-facing wiring; "sharing" is capability-gated by design.

### Q2: "The MCP and CLI part — what about it?"
- **CLI** (`cmd/cerberus`): real, talks to the daemon over RPC. Command set (from the web docs, grounded in code): `status, doctor, nodes, run, devices, wallet, caps, components, conflicts, economy, metrics, version`. Confirm each works against a running daemon (esp. `run` — does the CLI `run` use the same gateway/exec path that #7 fixes?).
- **MCP server** (`cmd/cerberus-mcp`): real, uses `github.com/modelcontextprotocol/go-sdk`; resolves the daemon via the discovery manifest (`resolve.go`) + reads the token via `auth.LoadToken`. `.mcp.json` / `.cursor/mcp.json` exist for auto-discovery from Claude Code/Cursor. Confirm what tools it exposes and whether they work end-to-end (it likely wraps the same daemon surfaces; if it exposes a "run" tool it inherits the #7 issue until fixed).
- **To answer the user:** run `bin/cerberus.exe help`, try `cerberus run`/`cerberus doctor`/`cerberus devices` against a live daemon, and inspect `cmd/cerberus-mcp/tools.go` for the MCP tool list. Give a plain Yes/No per capability.

## 4. Key architecture facts (so you don't re-derive)
- **Gateway** (`daemon/gateway`): `NewGateway(executor, authz)`; `dispatch()` → `executor.Dispatch/Resolve` → `result.Output` becomes the chat/text content. Seams: `SetOnDispatch` (history #8), `recordSettlement`/`SetSettler`/`PricingPolicy` (wallet #10), `RegisterModel`. Routes: `/v1/chat/completions`, `/v1/completions`, `/v1/models`.
- **Executor** (`daemon/wasm`): `NewExecutor(module)` runs a fixed module via wazero, resource-governed (`RunI32`/now `RunI32Any`). In the daemon it's `wasm.NewExecutor(e2enode.HelloShardWASM())` (`cmd/cerberusd/main.go` ~line 296-333).
- **Status/API** (`daemon/api`): `routes.go` serves `/api/v1/{status,devices,workloads,conflicts,conflicts/resolve,cap/revoke,devices/grant}`; `api.go` has the status Snapshot + `Actions`/`ListGetters` seams.
- **Tray app** (`tray/src-tauri/src/lib.rs`): Rust shell; commands call the daemon's HTTP surfaces (manifest-first URL resolution via `daemon.json`). One-click daemon: `spawn_daemon()` in `setup()` spawns the bundled `cerberusd` sidecar (`bundle.externalBin` + `tray/build-sidecar.mjs` builds the triple-named binary), killed on `RunEvent::Exit`. UI panels: Overview/Mesh/Devices/Workloads/Metrics/Wallet/Conflicts + settings/theme/search. Wolf logo = `tray/src/assets/wolf.svg`.
- **Multi-machine** (DONE): daemon binds `0.0.0.0` (`cmd/cerberusd --mesh-listen`, default routable; `CERBERUS_MESH_LISTEN` read in `daemon/system.meshListenAddrs`), logs `mesh: dialable at …`, `--peer <multiaddr>` bootstrap. Same-LAN = mDNS auto; cross-network = Tailscale + `--peer`. Proven live.

### Two-daemon local test (for #11 cross-node + device sharing)
Run two isolated daemons on one box (see the pattern in `test/chaos/real_process_test.go`): set `AppData`/`XDG_CONFIG_HOME`/`HOME` to separate temp dirs per instance, distinct `--api-addr/--gateway-addr/--metrics-addr/--rpc-addr`, `--mesh-listen /ip4/0.0.0.0/udp/0/quic-v1`, and give node B `--peer <node A dialable multiaddr>`. Node A's LAN addr on this box is `192.168.0.100`.

## 5. Recommended order for the new chat
1. Verify + commit the #7 fix (§2), push, sync main.
2. Verify #8 (should follow from #7); commit if any change.
3. Answer Q1 + Q2 for the user (investigate device-sharing wire + MCP/CLI) — these are owed and don't require code.
4. Do #10 (wallet transactions) and #11 (conflict detect/resolve E2E).
5. After each: build/test/e2e green, commit as hash066, push both branches, watch CI.

## 6. Housekeeping
- Leftover local processes may be running (old `cerberusd`, preview servers on :8091/:8092). Kill with `Get-Process cerberusd,tray | Stop-Process -Force` when starting fresh.
- One stale agent worktree dir (`.claude/worktrees/agent-a052b4ccf8c0dd794`) couldn't be deleted (file lock) — harmless, `git worktree prune` later.
- Installers already build: `cd tray && npm run tauri build` → `tray/src-tauri/target/release/bundle/{msi,nsis}/…` (~25 MB with the bundled daemon). All-OS via the `desktop` job in `.github/workflows/release.yml` on a `v*` tag.
