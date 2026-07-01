# Getting Started with Cerberus

> From zero to a running node, a workload through the gateway, and a second machine
> pooled into your mesh. Everything here is grounded in what the daemon actually
> does today — see the [maturity notes](#9-what-is-and-isnt-real-yet) for the honest edges.

Cerberus is a **zero-trust, masterless distributed hypervisor**: you launch a small
daemon on each machine you own, they discover each other on your LAN with no
configuration, and software running on the mesh can only touch resources it has been
handed an unforgeable **capability** for. This guide gets you to a working single
node, then a two-node mesh.

- New to the project? Read the one-paragraph pitch in the [README](../README.md).
- Want the full command reference? See [docs/cli.md](cli.md).
- Want to call it like an LLM API? See [docs/gateway.md](gateway.md).
- Want the concepts (capabilities, profiles, pooling)? See [docs/user-guide.md](user-guide.md).

---

## 1. Prerequisites

Cerberus is one Go module (control plane) plus one Rust workspace (capability
kernel / runtime). For the default build you only need Go — the daemon builds
pure-Go with `CGO_ENABLED=0` and needs no C toolchain.

| Tool | Version | Needed for |
|---|---|---|
| **Go** | 1.22+ | the daemon, CLI, gateway, mesh (required) |
| **Rust** (cargo) | stable | the `core/` crates + `cargo test` (recommended) |
| **git** | any | cloning |
| `task` | v3 (optional) | the `task build/test/demo` shortcuts |
| PowerShell 7+ | optional | the Windows cgo kernel build (`build/ffi.ps1`) |

Install the optional task runner:

```bash
go install github.com/go-task/task/v3/cmd/task@latest
```

> **Platforms.** The daemon is developed and tested on Windows and builds for
> macOS/Linux from the same source. The mesh, gateway, status API, metrics, CLI,
> and WASM execution are cross-platform. Hardware-bound features (GPU dispatch,
> FUSE/WinFsp mounts, OS microphone/speaker capture, the Tauri GUI) are
> hardware- or OS-gated — see [§9](#9-what-is-and-isnt-real-yet).

---

## 2. Get the code and build

```bash
git clone https://github.com/hash066/Cerberus.git
cd Cerberus

# Build everything (Go + Rust):
task build
# …or without the task runner:
go build ./...
cargo build            # optional; builds the Rust core crates
```

Run the tests to confirm a green tree:

```bash
task test
# …or:
go test ./...
cargo test             # optional
```

**Prove it end-to-end.** The v0.1 acceptance demo spins up two real `cerberusd`
processes, has them discover each other over the real mesh, and runs a WASM shard
remotely from one to the other:

```bash
go run ./test/e2e
# …or: task demo
```

You should see the harness build the daemon, both nodes discover each other, and:

```
DEMO PASSED: hello-shard.wasm ran remotely on worker and returned 1337.
```

That `1337` is a real WebAssembly module executed on the *other* node and its
result returned over a capability-gated stream. It is the smallest complete slice
of the whole system.

---

## 3. First run: start the daemon

Launch the headless daemon:

```bash
go run ./cmd/cerberusd
# or build once and run the binary:
go build -o cerberusd ./cmd/cerberusd && ./cerberusd
```

On startup you'll see something like:

```
cerberusd v0.1.0  profile=open_mesh  kernel=pure-go-stub  sample-cap=1
cerberusd: control plane up (v0.1 skeleton).
cerberusd: ledger ready (operator balance=1000000, profile=open_mesh)
cerberusd: composed system up (mesh + telemetry + scheduler + 9P under supervisor)
cerberusd: 9P control plane on <addr>; QUIC data plane on <addr> (open .../ctl mints a data-plane grant)
cerberusd: revocation gossip active (sys/revocations topic)
operator token written to <config-dir>/cerberus/operator.token (CLI reads it; or set CERBERUS_TOKEN)
Starting Gateway on :8080 (Bearer token required)
Starting status API on 127.0.0.1:7777 (Bearer token required)
Starting metrics on 127.0.0.1:7779 (/metrics token-gated; /healthz /readyz open)
Starting RPC server on 127.0.0.1:9092
```

What just came up on this node:

| Surface | Address | Auth | Purpose |
|---|---|---|---|
| **Gateway** | `:8080` | Bearer token (`exec`) | OpenAI-compatible HTTP front door ([docs/gateway.md](gateway.md)) |
| **Status API** | `127.0.0.1:7777` | Bearer token (`read`) | live JSON status for the tray/dashboard (`/api/v1/status`); `/healthz` open |
| **Metrics** | `127.0.0.1:7779` | `/metrics` Bearer (`read`); `/healthz`, `/readyz` open | Prometheus metrics + health probes |
| **RPC** | `127.0.0.1:9092` | Bearer token (in the RPC call) | the control socket the `cerberus` CLI talks to |
| **Mesh** | libp2p/QUIC + mDNS | capability-gated | zero-config LAN peer discovery + data plane |

Leave this terminal running. Open a second terminal for the CLI and curl.

> **Profiles.** `cerberusd` defaults to `-profile=open_mesh` (the compute economy
> is on; the daemon mints genesis credits). Use `-profile=sealed` for the
> economy-off, attestation-on mode. See
> [docs/user-guide.md](user-guide.md#two-profiles-open_mesh-vs-sealed).

---

## 4. The operator token

Cerberus has **no ambient authority** — every network surface above requires a
capability token. On first start the daemon mints an **operator token** (subject
`operator`, `admin` rights, 24-hour TTL) and writes it to your OS config directory:

| OS | Path |
|---|---|
| **Windows** | `%AppData%\cerberus\operator.token` |
| **macOS** | `~/Library/Application Support/cerberus/operator.token` |
| **Linux** | `$XDG_CONFIG_HOME/cerberus/operator.token` (usually `~/.config/cerberus/operator.token`) |

The CLI reads this file automatically. To use the token in a shell (for curl,
Python, etc.), load it into an environment variable:

```bash
# Linux / macOS
export CERBERUS_TOKEN="$(cat ~/.config/cerberus/operator.token)"
```

```powershell
# Windows (PowerShell)
$env:CERBERUS_TOKEN = Get-Content "$env:AppData\cerberus\operator.token"
```

`$CERBERUS_TOKEN` takes precedence over the file, so you can also point a client
at a **narrower** token you minted for it (see the capability model in
[docs/user-guide.md](user-guide.md#capabilities-in-plain-terms)).

Notes:
- The token is written `0600` (owner-only). Treat it like an SSH key.
- The signing key (`issuer.key`, in the same config dir) is persisted, so tokens
  minted before a restart still verify afterwards.
- The operator token expires after 24h; restart the daemon (or mint a fresh one)
  to rotate it.

---

## 5. Talk to the daemon: `cerberus status`

In your second terminal:

```bash
go run ./cmd/cerberus status
# …or, if you built it: ./cerberus status
```

Expected output:

```
Cerberus Daemon Status
Version: 0.1.0
State:   Running (Power: AC, Battery: 100.0%)
Auth:    authenticated as "operator"
```

If you get `no capability token found`, the daemon either isn't running or hasn't
written the token yet — start `cerberusd` first, or set `$CERBERUS_TOKEN`.

The full CLI reference (every command, flags, output) is in [docs/cli.md](cli.md).

---

## 6. Your first workload: call the gateway

The gateway is an **OpenAI-compatible** HTTP endpoint on `:8080`. Every request
must carry the operator token as a Bearer credential. The simplest call:

```bash
curl -sS http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $CERBERUS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
        "model": "cerberus-shard",
        "messages": [{"role": "user", "content": "hello"}]
      }'
```

You'll get an OpenAI-shaped response:

```json
{
  "id": "chatcmpl-cerberus",
  "object": "chat.completion",
  "created": 1751000000,
  "model": "cerberus-shard",
  "choices": [
    { "index": 0, "message": { "role": "assistant", "content": "..." } }
  ]
}
```

> **What's actually running.** The gateway maps your request to a Cerberus
> `ComputeTask` and dispatches it through the **real WASM executor** (wazero). In
> v0.1 the wired component is the demo `hello-shard` module, so the `content` you
> get back is that component's output — **not** text from a large language model.
> The value here is that the *path* is real and production-shaped: capability auth,
> request validation, `ComputeTask` dispatch, and an OpenAI-compatible envelope
> that drops into any SDK. Wiring a real LLM component behind it is a
> component-swap, not a rewrite. See [docs/gateway.md](gateway.md) for the full
> API (including a Python `openai` client example) and current limits.

---

## 7. View metrics and health

The metrics server on `127.0.0.1:7779` exposes Prometheus metrics plus liveness
and readiness probes:

```bash
# Liveness (open, no token) — 200 "ok" while the process serves HTTP:
curl -sS http://127.0.0.1:7779/healthz

# Readiness (open, no token) — 200 "ready" once the mesh is up, else 503:
curl -sS http://127.0.0.1:7779/readyz

# Prometheus metrics (token-gated: read):
curl -sS http://127.0.0.1:7779/metrics -H "Authorization: Bearer $CERBERUS_TOKEN"
```

You'll see counters/gauges including the live **peer gauge** (updated every 5s
from the real fabric). The richer JSON status the desktop tray consumes lives on
the status API:

```bash
curl -sS http://127.0.0.1:7777/api/v1/status -H "Authorization: Bearer $CERBERUS_TOKEN"
```

```json
{
  "version": "0.1.0",
  "profile": "open_mesh",
  "kernel": "pure-go-stub",
  "uptime_sec": 42,
  "mesh_up": true,
  "peers": [],
  "operator_balance": 1000000,
  "power": { "source": "AC", "battery_pct": 100, "lid": "open", "hint": "awake" }
}
```

---

## 8. Connect a second node (LAN discovery)

This is the moment Cerberus becomes a *mesh*. Discovery is **zero-config**: a
daemon advertises and browses the `_cerberus` mDNS service on the local subnet and
connects to peers it finds over libp2p/QUIC.

**On a second machine on the same LAN**, clone/build and start the daemon:

```bash
go run ./cmd/cerberusd
```

Within a few seconds the two daemons discover each other. Confirm from either node
by checking the `peers` array in the status API:

```bash
curl -sS http://127.0.0.1:7777/api/v1/status -H "Authorization: Bearer $CERBERUS_TOKEN"
# → "mesh_up": true, "peers": ["<peer addr>", ...]
```

…or watch the peer gauge in `/metrics` climb.

**No second physical machine?** You can still see the two-node behaviour on one
box with the acceptance demo, which spawns two daemon processes and runs a shard
across them:

```bash
go run ./test/e2e     # prints: DEMO PASSED: ... returned 1337.
```

Notes on discovery:
- Both machines must be on the **same L2 subnet** for mDNS multicast to reach.
  Many corporate/guest Wi-Fi networks block multicast and client-to-client
  traffic — use a home/office LAN or a wired switch if peers don't appear.
- Discovery uses standard mDNS (`224.0.0.251` / `ff02::fb`, UDP `5353`).
- There is **no master node** and no cloud account — peers coordinate directly.

---

## 9. What is and isn't real yet

Cerberus is honest about maturity by design. This is what you can rely on today
versus what is a labelled stub or hardware-gated. (Full matrix:
[VISION-AND-ROADMAP.md](../VISION-AND-ROADMAP.md), [HANDOFF.md](../HANDOFF.md).)

**Real and tested (works on your machine now):**
- Zero-config LAN discovery + libp2p/QUIC mesh (mDNS), masterless.
- Ed25519 capability tokens gating the gateway, status API, metrics, and RPC.
- Real WebAssembly execution (wazero in Go, wasmi/Wasmtime in Rust) — the e2e
  runs a shard remotely and returns `1337`.
- Durable state (bbolt): issuer key, revocations, the credit ledger, and CRDT
  checkpoints survive a restart.
- Cost-model scheduler with reroute/standby; lid-drop → reroute wiring.
- Distributed revocation gossip (revoke on node A → denied on node B).
- Metrics/health endpoints; status API; the OpenAI-compatible gateway path.

**Stub or hardware/OS-gated (documented, not faked):**
- **`cerberus run`** — a stub today (prints a placeholder). Use the **gateway**
  path for real compute dispatch. See [docs/cli.md](cli.md#run).
- **Gateway content** — real dispatch, but the wired component is the demo shard,
  not an LLM (see §6).
- **GPU dispatch (wgpu/MLX)** — a real wgpu backend exists (and has run on the
  dev box's NVIDIA GPU) but is off by default and needs real GPU hardware to
  validate honestly.
- **FUSE/WinFsp mounts** — the 9P namespace and QUIC data plane are real; mounting
  a peer's device as a local drive needs a kernel driver (labelled stub).
- **OS microphone/speaker capture** — the network audio *transport* is real; the
  CoreAudio/WASAPI/PipeWire capture source is a labelled stub.
- **Tauri desktop GUI** — real code, but not launched/verified headlessly here;
  needs the Tauri toolchain + WebView2 and a real desktop session.
- **Frontier (research bets, opt-in):** zk-WASM proof-of-inference, host-TEE
  memory shielding, RDMA-over-Thunderbolt.

---

## 10. Troubleshooting

**`no capability token found: set $CERBERUS_TOKEN or start cerberusd`**
The daemon isn't running or hasn't written the token. Start `cerberusd` first, or
export `CERBERUS_TOKEN` from the token file (see [§4](#4-the-operator-token)).

**`Failed to connect to cerberusd` / RPC dial errors**
`cerberusd` isn't listening on `127.0.0.1:9092`. Make sure it's running in another
terminal and finished startup (you saw "Starting RPC server on 127.0.0.1:9092").

**Gateway returns `401 unauthorized` / "missing bearer token"**
Your `Authorization: Bearer <token>` header is missing, malformed, or the token
lacks the required right (`exec` for the gateway, `read` for status/metrics), or
it expired (24h TTL) or was revoked. Re-read the token from disk or restart the
daemon to mint a fresh one.

**Port already in use (`:8080`, `7777`, `7779`, `9092`)**
Another process (or a previous `cerberusd`) holds the port. Stop the other process,
or stop the stale daemon. These ports are currently fixed in the v0.1 daemon.

**Second node doesn't appear in `peers`**
- Confirm both nodes are on the **same subnet** and mDNS/multicast isn't blocked
  (common on corporate/guest Wi-Fi). Try a home LAN or wired switch.
- Check the firewall isn't blocking UDP `5353` (mDNS) or the QUIC ports.
- Give it a few seconds; discovery is periodic. Watch the `/metrics` peer gauge or
  `/api/v1/status` `peers`.

**`readyz` returns 503 "not ready: mesh down"**
The composed mesh failed to start (check the daemon log for a `compose system
failed` line). The rest of the daemon (gateway, RPC, status) still runs; readiness
just reflects the mesh.

**Rust/cgo build issues (`-tags ffi`)**
The default build is pure-Go and needs no C toolchain. Only the optional real Rust
kernel (`build/ffi.ps1`) needs zig + the `x86_64-pc-windows-gnu` target — see
[docs/ffi.md](ffi.md).

---

## Next steps

- **[docs/cli.md](cli.md)** — every `cerberus` command with examples.
- **[docs/gateway.md](gateway.md)** — the OpenAI-compatible API (curl + Python).
- **[docs/user-guide.md](user-guide.md)** — capabilities, profiles, peripheral
  pooling, the desktop app, and the economy.
- **[ARCHITECTURE.md](../ARCHITECTURE.md)** — the canonical system spec.
- **[VISION-AND-ROADMAP.md](../VISION-AND-ROADMAP.md)** — where this is going.
