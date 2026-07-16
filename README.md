# Cerberus

**A zero-trust distributed hypervisor for the machines you already own.**

Install one daemon on each of your Windows/Mac/Linux boxes. They discover each
other on your LAN and become a single mesh where **everything is a
capability-gated device**: run sandboxed WASM on a peer's CPU, dispatch a compute
kernel to a peer's GPU, store files erasure-coded across everyone's disks, stream
your microphone to another machine's speaker — every one of those actions
presents an unforgeable, signed, revocable capability. No master node, no cloud
account, no passwords, no ambient authority.

<p>
  <img alt="status: v0.1 beta" src="https://img.shields.io/badge/status-v0.1%20beta-blue">
  <img alt="Go 1.22+" src="https://img.shields.io/badge/Go-1.22%2B-00ADD8">
  <img alt="Rust" src="https://img.shields.io/badge/Rust-stable-orange">
  <img alt="platforms" src="https://img.shields.io/badge/platforms-Windows%20%7C%20macOS%20%7C%20Linux-lightgrey">
  <img alt="license" src="https://img.shields.io/badge/license-NONE%20YET-red">
</p>

> **No licence yet.** There is no `LICENSE` file in this repo, so default
> copyright applies and you do not have permission to redistribute or use this
> in your own work. Read it, run it, break it, file issues — but a licence has
> to land before this is open source in any sense that matters.

If you know [exo](https://github.com/exo-explore/exo): exo pools your devices to
serve an LLM, and it is good at that. Cerberus is a different category — it pools
the **machines themselves** (compute, GPU, storage, audio) behind an
object-capability security model, so you can safely hand pieces of your hardware
to other people and to AI agents. **Cerberus does not serve LLMs today**; if that
is what you want, use exo. [Comparison below.](#cerberus-vs-exo)

---

## What works today (v0.1 beta — honest)

Everything in the **Real** column runs today and is exercised by tests or an
e2e harness in this repo. Nothing below is aspirational; the linked code is the
claim.

| Capability | Status | Where |
|---|---|---|
| Masterless mesh: mDNS auto-discovery on LAN + explicit `--peer` bootstrap (Tailscale/WireGuard across networks) | **Real** | `daemon/mesh`, `cmd/cerberusd` |
| Capability kernel: Ed25519 signed tokens — mint, attenuate, revoke; revocations gossip mesh-wide and survive restart | **Real** | `daemon/auth`, `core/ocap` |
| Capabilities are **scoped to their resource**: a cap minted for `/cer/dev/vram/local/0` is refused on `/cer/dev/gpu/local/0` | **Real** in the kernel that ships (pure-Go, `kernel=pure-go-stub`), enforced on the 9P walk/open path, with a regression test. **Two residuals, stated plainly below the table.** | `contract/go/stub/stub.go:113`, `daemon/ninep/capscope_proof_test.go` |
| Remote WASM execution: `cerberus run x.wasm --on <peer>` in a deny-by-default sandbox (wazero), content-addressed (CID) | **Real** | `cmd/cerberusd/rpc.go`, `daemon/compute`, `daemon/wasm` |
| 9P device namespace: `/cer/dev/{gpu,cpu,vram,audio}`, `/cer/fs` — walk/open is capability-checked; opening a device `ctl` mints a QUIC data-plane grant | **Real** | `daemon/ninep`, `daemon/system/system.go` |
| QUIC data plane with mutual TLS + PeerID pinning; bulk bytes never ride the control plane | **Real** | `daemon/dataplane` |
| GPU kernel dispatch, local **and** cross-node (`cerberus gpu … --on <peer>`), gated by a signed exec capability | **Real** | `daemon/gpu`, `daemon/mesh/gpu.go` |
| Physical-GPU backend (wgpu → Vulkan/DX12/Metal) | **Partial** — from-source build (`task build:gpu`); default binaries compute on CPU and honestly report `backend: cpu-software` | `core/runtime`, [docs/gpu.md](docs/gpu.md) |
| Pipeline-parallel inference across nodes: scheduler places layer shards on peers, activations hand off over the QUIC data plane | **Real**, but the model is a **4-layer × 4-dim MLP fixture**, not a language model — no tokenizer, weights, KV-cache or sampler | `daemon/system/pipeline.go`, `test/pipeline_e2e` |
| LLM inference through Cerberus (llama.cpp) | **Stub — not wired.** `daemon/llama` (supervise `llama-server`/`ggml-rpc-server`, cap-gated RPC tunnel) is written and unit-tested, but **no binary imports it** — `grep -r cerberus/daemon/llama` finds zero importers, so it does not run in `cerberusd` | `daemon/llama` (unwired) |
| Cross-node audio: your mic → a peer's speaker (`cerberus audio play --on <peer>`), capability-gated | **Real** on Windows (WASAPI) and Linux (PulseAudio native protocol, pure Go, no cgo). macOS CoreAudio is written but **never compiled and never run** — it is behind `//go:build darwin && cgo && cerberus_coreaudio`; default macOS builds get an honest stub | `daemon/audio/os_windows.go`, `os_linux.go`, `os_darwin.go` |
| Distributed filesystem `/cer/fs`: Reed-Solomon erasure coding, shards scattered to peers, write on node A / read on node B | **Real** via CLI (`fs put`/`get`/`ls`) and the Server API | `daemon/dfs`, `daemon/system` |
| Mount the 9P namespace as a real host filesystem (`cerberusd -mount`) | **Partial.** Linux FUSE: **real**, verified in WSL2 (`/proc/mounts` shows `fuse.cerberus`). Windows: code path exists (cgofuse nocgo → WinFsp) but **unverified — needs the WinFsp kernel driver, which we have not tested against**. macOS/BSD: documented stub. **`/cer/fs` files are not browsable through a mount** — see the note below the table | `daemon/ninep/mount_linux.go`, `mount_windows.go` |
| Telemetry-driven placement + lid-drop reroute (sleep imminent → checkpoint → promote standby) | **Real** (baseline) | `daemon/scheduler`, `daemon/lifecycle` |
| VRAM telemetry: the scheduler's `--tensor-split`-grade number | **Real** and source-tagged — matches `nvidia-smi` exactly on an RTX 3050 Laptop (4096/3861 MiB). A card it cannot measure reports **unknown** (0/0, infeasible for placement), never a plausible default | `daemon/gpu/vramprobe.go`, [docs/vram.md](docs/vram.md) |
| OpenAI-compatible gateway (`/v1/chat/completions` incl. SSE streaming, `/v1/models`) — Bearer capability token required | **Partial, and currently misleading — see the warning below.** The HTTP surface, auth and SSE framing are real; `/v1/models` lists WASM shards only. **No LLM is served** | `daemon/gateway` |
| MCP server: Claude Code / Cursor drive the mesh (12 tools: run workloads, list nodes, mint/revoke caps, …) | **Real** | `cmd/cerberus-mcp`, [docs/mcp.md](docs/mcp.md) |
| Desktop tray app (Tauri v2): bundles + auto-starts the daemon, dashboard for nodes/devices/workloads/wallet/conflicts | **Real** (installers unsigned — beta) | `tray/` |
| CRDT agent memory with belief-conflict surfacing (contradictions go to a human, never silent last-writer-wins) | **Real** | `daemon/state`, `core/crdt` |
| Compute economy: durable ledger, per-run transaction log, fraud-proof `challenge` that slashes | **Partial** — usage accounting only, **no real value moves** | `daemon/ledger`, `daemon/economy` |
| zk-WASM proof-of-inference | **Stub** — documented design, not implemented | [ARCHITECTURE.md §8](ARCHITECTURE.md) |
| RDMA-over-Thunderbolt data plane | **Stub** — documented design, not implemented. `EndpointRDMA` is a constant no code path uses, and a **test fails the build if any interface ever claims RDMA** | `daemon/hostinfo/hostinfo_test.go:63` |
| TEE memory shielding (TDX/SEV/Enclave) | **Stub** — documented design, not implemented | [ARCHITECTURE.md §8](ARCHITECTURE.md) |

> [!WARNING]
> **`/v1/chat/completions` will answer for a model it does not have.** Today the
> gateway resolves an unknown model name to a WASM component CID, falls through
> to the seeded `hello-shard`, and returns **HTTP 200** with the shard's output
> as the assistant message. `POST {"model":"gpt-4o"}` returns
> `"content": "1337"`. The `usage` field is worse: `prompt_tokens` is the
> **character count** of your prompt and `completion_tokens` is the **byte
> length** of the reply ([`daemon/gateway/chat.go:186`](daemon/gateway/chat.go))
> — there is no tokenizer anywhere in that path. Do not point an OpenAI client
> at this expecting an LLM, and do not trust those token counts. This is a known
> bug, tracked, and is the reason the gateway row above says *Partial*.

**Two capability-scoping residuals**, stated because they are the difference
between the claim and the whole truth:

1. **The mesh's audio / shard / compute / GPU protocols do not scope.** They
   verify the signature and the *right*, then discard the grant
   (`daemon/mesh/audio.go:214`, `shard.go:227` — literally `if _, verr := …`).
   `grep grant.Resource daemon/mesh/*.go` finds only `llamarpc.go`, which is the
   one protocol that does compare the resource
   ([`llamarpc.go:329`](daemon/mesh/llamarpc.go)). So a peer holding *any* valid
   signed cap carrying the required right passes those four gates.
   `auth.Verify` cannot close this: the resource is not one of its arguments.
2. **The Rust kernel behind `-tags ffi` cannot scope at all.** The frozen C ABI
   is `cerberus_cap_verify(handle, op, now)` — the resource never crosses the
   boundary ([`daemon/ffi/kernel_ffi.go:170`](daemon/ffi/kernel_ffi.go)). This
   does not affect release binaries, which are pure Go (`kernel=pure-go-stub`),
   but `-tags ffi` builds silently lose scope enforcement.

The rule this repo is built under ([CLAUDE.md](CLAUDE.md)): a feature either
works or tells you it isn't wired — nothing is faked. The GPU command prints the
backend that *actually* ran your kernel; a card whose VRAM we can't measure
reports `unknown` rather than a plausible default; unsigned installers say so.
The v0.1 mock inference backends were **deleted rather than repaired** — asking
for `--backend llamacpp` now fails loudly instead of quietly running a 4-float
toy under a real engine's name ([`daemon/inference/backend.go:54`](daemon/inference/backend.go)).

---

## 60-second quickstart (one machine)

**From a release** (Windows): grab either asset from the
[latest release](https://github.com/hash066/Cerberus/releases/latest) —
the `.msi` installer (tray app, daemon auto-starts) or the CLI zip
`cerberus_<version>_windows_amd64.zip` (unzip, then run `cerberusd.exe`
yourself). Installers are unsigned for now: SmartScreen → **More info → Run
anyway**.

**From source** (any OS, Go 1.22+ — the default build is pure Go, no C
toolchain):

```bash
git clone https://github.com/hash066/Cerberus.git
cd Cerberus

# Prove the whole thing end-to-end first: two daemons form a mesh and one
# executes a WASM shard for the other (prints "... returned 1337").
go run ./test/e2e

# Now run a real daemon (leave it running; it writes an operator
# capability token to your OS config dir — the CLI picks it up automatically)
go run ./cmd/cerberusd
```

In a second terminal:

```bash
go run ./cmd/cerberus status      # daemon health, identity, mesh, balance
go run ./cmd/cerberus devices     # the capability-gated 9P device namespace
go run ./cmd/cerberus gpu vector-add 1,2,3 4,5,6
#   vector-add(a,b) = [5 7 9]
#   backend: cpu-software         <- honest: no GPU build, real CPU compute
go run ./cmd/cerberus fs put README.md && go run ./cmd/cerberus fs ls
go run ./cmd/cerberus pipeline-run   # split-MLP FIXTURE; prints where each stage ran
#   Model:   split-mlp-demo
#   Backend: cpu-software
#     layers 0-1 on 6bd905b1… (remote) 85.2ms
#     layers 2-3 on 61939825… (local)   0s
#   Content: [split-mlp fixture cpu-software] activation: [0.8165, 0.8181, …]
```

That `pipeline-run` output is the real thing to look at, and also the thing to
read carefully: the *orchestration* is real (the scheduler chose those nodes from
live telemetry, and the activation crossed a QUIC data plane to a different
machine), and the *model* is a 4×4 MLP fixture. Both halves of that sentence are
load-bearing.

**Two machines is the point** — remote WASM exec, dispatching kernels to a
peer's GPU, files written on A read on B, your mic on B's speaker:
**[QUICKSTART.md](QUICKSTART.md)** has the exact copy-paste steps (LAN and
Tailscale).

---

## How it fits together

```
        you                            your AI agents
  ┌───────────┐ ┌──────────┐   ┌───────────────┐ ┌────────────────────┐
  │ cerberus  │ │ tray app │   │ cerberus-mcp  │ │ any OpenAI SDK     │
  │   (CLI)   │ │ (Tauri)  │   │(Claude/Cursor)│ │ base_url=:8080/v1  │
  └─────┬─────┘ └────┬─────┘   └───────┬───────┘ └─────────┬──────────┘
        └────────────┴──────┬──────────┴───────────────────┘
                            │ every call presents a capability token
  ┌─────────────────────────▼─────────────────────────────────────────┐
  │                      cerberusd  (Go daemon)                       │
  │                                                                   │
  │  OCap kernel (Rust): Ed25519 caps — mint / attenuate / revoke,    │
  │      revocation gossip mesh-wide, issuer key in the OS keychain   │
  │  wazero WASM sandbox · telemetry-driven scheduler · CRDT memory   │
  │  9P namespace:  /cer/dev/{gpu,cpu,vram,audio}   /cer/fs           │
  │      (walk/open is cap-checked; opening ctl mints a data grant)   │
  └───────┬───────────────────────────────────────────┬───────────────┘
          │ CONTROL: libp2p/QUIC                      │ DATA: QUIC
          │ mDNS discovery, signed caps,              │ mTLS, PeerID-pinned
          │ small messages only                       │ tensors/files/audio
  ┌───────▼─────────┐        ┌─────────────────┐      ▼
  │ cerberusd       │  ...   │ cerberusd       │   (bulk bytes never
  │ (your other PC) │        │ (friend's box)  │    touch the control plane)
  └─────────────────┘        └─────────────────┘
```

Three design decisions carry the system
([ARCHITECTURE.md](ARCHITECTURE.md) is the full spec):

1. **Capabilities, not identities.** There are no user accounts and no ACLs.
   To touch a resource you present a signed token that names exactly that
   resource and rights; you can hand a narrower copy to someone else
   (`cerberus caps attenuate`) and kill the whole chain later
   (`cerberus caps revoke`, gossiped to every node).
2. **Control plane ≠ data plane.** Discovery, grants, and scheduling ride
   libp2p/QUIC as small signed messages. File shards, activations, and audio
   frames ride a separate mTLS QUIC data plane that a control-plane grant
   unlocks. Opening `/cer/dev/gpu/<peer>/0/ctl` returns a data-plane endpoint,
   never bytes.
3. **WASM as the unit of compute.** Workloads are WebAssembly, so an ARM Mac
   and an x86 PC run the same bytes, and the sandbox is the deny-by-default
   import set — a guest literally cannot name a resource it wasn't granted.

---

## Cerberus vs exo

[exo](https://github.com/exo-explore/exo) is the obvious comparison and it's a
good project — if your goal is "run a big LLM across my Macs tonight," use exo;
its model-parallel LLM serving is real and that is its entire focus. Cerberus
is a bet one layer down: that the interesting primitive is not a shared model
but a shared, *securable* machine.

| | exo | Cerberus |
|---|---|---|
| Pools | Your devices' memory/compute to serve **one model** | The machines themselves: WASM compute, GPU kernels, files, mic/speaker |
| LLM inference today | Runs real models across your devices. **We have not benchmarked exo and make no claim about its numbers** — assume it does the job it advertises | **None.** Cross-node pipeline orchestration is real but runs a 4-layer MLP fixture. The llama.cpp integration is written and unwired. This is not an LLM product |
| Trust between nodes | Trusted-LAN assumption | Zero-trust: every cross-node call carries a signed, attenuable, revocable Ed25519 capability — **with the four unscoped mesh protocols named above** |
| Workload isolation | Python processes | WASM sandbox; imports are the security boundary |
| Peripheral sharing | — | 9P namespace: a peer's GPU/CPU/audio/fs as quota'd devices; real FUSE mount on Linux |
| Delegation / revocation | — | First-class: `caps mint/attenuate/revoke`, revocation gossip |
| Agent surface | ChatGPT-compatible API | MCP server + an OpenAI-compatible gateway that **does not serve an LLM** (see the warning above) |
| Platform center of gravity | macOS / Apple Silicon, Linux | Windows + Linux (audio real on both); macOS builds but its audio backend has never been compiled |
| Stack | Python | Go control plane + Rust capability kernel, static binaries |

Different question, different tool: exo asks *"how do I fit a 70B model on my
devices?"* Cerberus asks *"how do I safely let anything — friends' machines, my
own agents — use my hardware at all?"*

---

## Interfaces

Four ways to drive the same capability-gated daemon:

- **CLI** — `cerberus status | run | pipeline-run | nodes | devices | fs | gpu |
  audio | caps | wallet | conflicts | economy | metrics | doctor`
  ([docs/cli.md](docs/cli.md))
- **Tray app** — install-and-forget dashboard; bundles and auto-starts the
  daemon ([tray/](tray/))
- **MCP** — point Claude Code or Cursor at `cerberus-mcp` and your agent can
  discover nodes, run workloads, and mint/revoke capabilities — with a token
  you can scope down ([docs/mcp.md](docs/mcp.md))
- **OpenAI-compatible HTTP** — `base_url=http://localhost:8080/v1` with a
  Bearer capability token. It runs **WASM shards**, not language models, and
  today it will answer for any model name you send it — read the warning above
  before you point a client at it ([docs/gateway.md](docs/gateway.md))

## Documentation

| Doc | What it covers |
|---|---|
| **[QUICKSTART.md](QUICKSTART.md)** | two Windows machines, LAN + Tailscale, every cross-node feature |
| **[TESTERS.md](TESTERS.md)** | beta-tester guide: install, solo tour, honest status table |
| **[Getting Started](docs/getting-started.md)** | build from source, operator token, profiles, troubleshooting |
| **[CLI Reference](docs/cli.md)** | every `cerberus` command with examples |
| **[Gateway API](docs/gateway.md)** | the OpenAI-compatible surface |
| **[MCP server](docs/mcp.md)** | wiring Claude Code / Cursor to the mesh |
| **[GPU](docs/gpu.md)** | the honest backend model + the real-GPU (wgpu) build |
| **[ARCHITECTURE.md](ARCHITECTURE.md)** | canonical spec: schemas, invariants, maturity matrix (§8) |
| **[Vision & Roadmap](VISION-AND-ROADMAP.md)** | where this goes; what's real vs stub, by name |
| **[docs/verticals/](docs/verticals/)** | 11 low-level designs (ocap kernel, mesh, CRDT, 9P, economy, …) |
| **[CLAUDE.md](CLAUDE.md)** | build conventions incl. the maturity-honesty rule |

Historical: the original narrative spec ("Volume I") is preserved at
[docs/research/volume-1-original-spec.md](docs/research/volume-1-original-spec.md).

## Repository layout

```
cmd/        cerberusd (daemon) · cerberus (CLI) · cerberus-mcp (MCP server)
daemon/     Go control plane: mesh scheduler ninep dataplane gateway auth
            dfs audio inference system state ledger economy telemetry metrics
core/       Rust: ocap kernel · wasmtime runtime · crdt · identity · economy · cabi (FFI)
contract/   frozen integration types (Go + Rust)      [do not edit casually]
proto/ components/wit/ schemas/                        [frozen contract sources]
tray/       Tauri v2 desktop app
test/       e2e harnesses: 2-node WASM exec · pipeline · peripherals · chaos · load
build/      release packaging, FFI/GPU build scripts, CI helpers
```

## Build & test

```bash
task build   # Go + Rust        (raw: go build ./... && cargo build)
task test    # unit tests       (raw: go test ./... && cargo test)
task demo    # 2-node mesh + remote WASM exec acceptance (raw: go run ./test/e2e)
task lint    # golangci-lint + clippy
task build:gpu   # Windows: daemon with the real wgpu GPU backend (docs/gpu.md)
```

No `task`? `go install github.com/go-task/task/v3/cmd/task@latest`, or run the
raw commands from [Taskfile.yml](Taskfile.yml). CI runs the test matrix on
Linux/macOS/Windows with `-race`, fuzz harnesses, and an SBOM gate.

## Maturity, honestly

This is a **v0.1 beta**. The capability kernel, mesh, sandbox, data plane,
device namespace, distributed FS, cross-node audio/GPU/pipeline paths, CLI,
MCP server, and tray app are real and running. The frontier pieces — zk-WASM
proof-of-inference, RDMA-over-Thunderbolt, TEE shielding — are **documented
stubs** ([ARCHITECTURE.md §8](ARCHITECTURE.md)), kept as designs rather than
faked as features.

Known gaps, in the order they'd embarrass us:

1. **The gateway answers for models it does not have**, with fabricated token
   counts (see the warning above). This is the sharpest edge in the repo.
2. **LLM inference does not run through Cerberus.** `daemon/llama` is written
   and tested; nothing imports it.
3. **Four mesh protocols verify rights but not resource scope**
   (audio/shard/compute/gpu — see the residuals above).
4. **`/cer/fs` is not browsable through a mount** — `ListChildren` doesn't
   enumerate fs files and a read-open over 9P returns `ENOSYS`
   ([`daemon/ninep/wire.go:256`](daemon/ninep/wire.go)). "Put a file, see it in
   Explorer, read it back" is **not** a thing Cerberus does today.
5. **Windows mount is unverified** (needs the WinFsp driver); macOS audio has
   never been compiled.
6. **No LICENSE file exists.** That is a real problem for anyone who wants to
   use this: with no licence, default copyright applies and you do not have
   permission to redistribute. It needs fixing before this is meaningfully open
   source. (Note for whoever picks it: WinFsp is GPLv3 and llama.cpp is MIT —
   both matter to the mount and llama paths respectively.)
7. Live VRAM quota accounting on the GPU device, and signed installers.

If you find a claim in this README the code doesn't back, that's a bug — file
it. Several of the items above are here because exactly that audit was run
against this file.
