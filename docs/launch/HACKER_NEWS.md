# Show HN draft — Hacker News launch

> **Draft for the maintainer to post manually.** Nothing in this file is
> published by tooling. Edit voice to taste, but keep every factual claim —
> each one is backed by a code path listed at the bottom. HN's antibody
> response is to overclaiming; ours is that the repo itself labels its stubs.

---

## Title

(80-char limit. Option A is the recommendation.)

**A.** `Show HN: Cerberus – a zero-trust distributed hypervisor for machines you own`

**B.** `Show HN: Cerberus – share your PCs' compute, GPU, files and mics as one mesh`

**C.** `Show HN: I built a capability-secured mesh that pools my own computers`

---

## Body

I've been building Cerberus: a daemon you install on the Windows/Mac/Linux
machines you already own. They discover each other over mDNS (or one explicit
`--peer` over Tailscale), form a masterless mesh, and after that every machine's
resources are devices in a capability-gated namespace: `/cer/dev/gpu`,
`/cer/dev/cpu`, `/cer/dev/audio`, `/cer/fs`.

What that buys you, concretely, today:

- `cerberus run app.wasm --on <peer>` — the module executes in a
  deny-by-default sandbox on the other machine and the real result comes back.
  In v0.1 guests literally cannot import anything: a wasm module that declares
  imports won't instantiate, so remote code gets pure compute, no fs/net/clock.
- `cerberus gpu vector-add 1,2,3 4,5,6 --on <peer>` — dispatches a kernel to
  the peer, which verifies an Ed25519-signed exec capability *before* running,
  and replies with the result plus the backend that actually computed it
  (`cpu-software` by default; `gpu-wgpu` if you build with the wgpu feature —
  the output never claims a GPU it didn't use).
- `cerberus fs put report.pdf` on machine A, `cerberus fs get` on machine B —
  Reed-Solomon erasure-coded shards scattered across peers, reconstructed on
  read.
- `cerberus audio play --on <peer>` — my mic playing out of another machine's
  speaker over the QUIC data plane. Real on Windows (WASAPI) and Linux
  (PulseAudio's native protocol, in pure Go — no cgo). The macOS CoreAudio
  backend is written but has **never been compiled**, and it's behind an opt-in
  build tag saying so.
- `cerberus pipeline-run` — the scheduler places layer shards on different
  nodes from live telemetry, and activations hand off over the data plane. The
  distribution is real. **The model is a 4×4 MLP fixture** — no tokenizer, no
  weights, no KV-cache, no sampler. It is not a language model.

- `cerberusd -llama-model model.gguf` — runs a real `llama-server`, so
  `/v1/chat/completions` serves a real model with a real tokenizer and real
  token counts. **Single-node only**, and new — treat it as beta inside a beta.
  Splitting a model *across* machines is not wired yet; see below, because the
  reason is more interesting than the feature.

On LLMs, three things I'd rather say up front than have found:

**One.** v0.1 shipped `llamacpp` and `mlx` "backends". They were mock transforms
wearing real engines' names, and one of them (`forward.cpp`) had **never been
compiled in its life** — it was protocol-broken in both directions and called an
API that doesn't exist in llama.cpp. Nobody noticed because nothing ever built
it. I deleted all of it rather than repair it; `--backend llamacpp` now fails to
your face, and a test pins the deletion so it can't crawl back.

**Two: splitting a model across machines — the actual headline feature — is not
wired.** Both halves are in the tree and neither reaches the other. A worker
(`-llama-worker`) serves tensor work over a capability-gated mesh session with
its `ggml-rpc-server` bound to loopback only, deliberately unreachable from the
LAN. A client (`-llama-rpc`) takes a raw `host:port` and dials straight past the
mesh gate — so it can't reach that worker, by construction. The forwarder that
bridges them (a loopback listener that pipes into the authenticated session) is
written and tested, and **no binary ever constructs one**. I'd rather you learn
that here than by trying it.

**Three, and this argues against the feature anyway:** testing upstream llama.cpp
directly (one machine, single run), a model split over `ggml-rpc` ran at **~45
tok/s vs ~396 tok/s** on a single node that could hold it. **Distributing is ~9×
slower.** That's structural — activations cross the network at every layer
boundary — not something I can tune away. So the pitch, when this does land, is
*"run a model that fits on no single machine you own, safely"*, **never** *"go
faster"*. If you came here for fast local inference you want plain llama.cpp or
exo, and I'd rather say that in the post than in a reply.

And a warning I won't soften: `-llama-worker` is off by default because turning
it on **grants code execution to any peer that can join your mesh** — today, any
machine on your LAN running cerberusd. Peers self-issue their own capabilities
(the gate's issuer resolver asserts only "the signer is the peer on this
stream"), mDNS auto-connects, there's no peer allowlist anywhere, and ggml-rpc
isn't a sandbox. Resource scoping stops capability *substitution*; it is not
authorization when the attacker mints the capability. That's the honest state of
it, and it's why the flag's help text says so at length rather than "peers
holding a capability".

The part I actually care about — and the part I'd most like this crowd to tear
apart — is the security model. There are no accounts, roles, or ACLs anywhere.
Every cross-boundary call (CLI→daemon, peer→peer, agent→gateway) presents a
signed capability token naming a resource and rights. You can hand someone a
strictly narrower token (`caps attenuate`: rights ⊆ parent, tighter caveats),
and revoke it later — revocations persist and gossip mesh-wide. The data plane
is separate from the control plane: opening a device's `ctl` file over 9P mints
a QUIC (mTLS, PeerID-pinned) grant, and bulk bytes never touch the control
plane. Agents get the same deal: there's an MCP server and an OpenAI-compatible
gateway, so Claude/Cursor or any SDK can drive the mesh with a token you scoped
down, not with your admin rights.

What's real vs. not (the repo enforces this distinction — CLAUDE.md forbids
faking maturity):

- **Real and tested:** the mesh, the signed-capability kernel + revocation
  gossip, remote WASM exec, cross-node GPU dispatch, the erasure-coded FS,
  cross-node audio (Windows + Linux), pipeline orchestration over a fixture, a
  real Linux FUSE mount of the namespace, the tray app, CLI, MCP server, CI
  with race + fuzz gates.
- **Partial:** physical-GPU backend needs a from-source build (default binaries
  honestly report `cpu-software`); the compute "economy" is a durable usage
  ledger with a working fraud-proof `challenge` — no real value moves; the
  Windows mount exists in code but is **unverified** (needs WinFsp).
- **Documented stubs, not implemented:** zk-WASM proof-of-inference,
  RDMA-over-Thunderbolt, TEE memory shielding. Design docs with "Frontier"
  stamped on them. The RDMA one is enforced, not just asserted: a test
  (`daemon/hostinfo/hostinfo_test.go:63`) **fails the build if any code ever
  claims RDMA**. I'd rather the test suite police my marketing than trust myself
  to.

Three things I'd rather you hear from me than find:

1. **`/v1/chat/completions` is a live footgun without `-llama-model`.** With no
   chat backend wired, it answers HTTP 200 for *any* model name —
   `{"model":"gpt-4o"}` returns `"1337"` (a WASM shard's output) — and the
   `usage` counts are fabricated from character/byte lengths, with no tokenizer
   in *that* path (`daemon/gateway/chat.go:186`). It should 404 an unknown model
   and omit `usage` it can't compute. It's the sharpest edge in the repo and I
   found it by auditing my own docs before posting this. (With `-llama-model`, a
   real llama-server serves it and the counts are llama.cpp's real ones — the
   bug is the fallback, not the llama path.)
2. **Until a few hours before this post, four mesh protocols verified rights but
   not resource scope.** The kernel always scoped correctly, but
   `daemon/mesh`'s audio/shard/compute/gpu gates checked the signature and the
   *right*, then discarded the grant without ever comparing `grant.Resource` —
   two of them literally `if _, verr := verify…`. Since compute and gpu both
   require `exec`, a peer holding a compute capability could open the GPU
   endpoint. That's cross-service capability substitution, and it made the
   central claim of this project false in the exact place it most needed to be
   true. It's fixed now — one shared gate (`daemon/mesh/capscope.go`) with a
   per-protocol denial test each, plus tests rejecting prefix-confusion and
   wildcards — but I'm telling you it existed because "we shipped a capability
   system that didn't check capabilities" is the most interesting thing an
   audit found, and you'd have found it in the git log anyway.
3. **There is no LICENSE file.** Default copyright applies, which means this
   isn't open source in any way that matters yet. Fixing it is on me.

Stack: Go control plane (libp2p/QUIC, 9P server, scheduler), wazero for
sandboxing, Tauri tray app. One correction I owe you up front, since the repo's
own docs got this wrong until I audited them: **releases ship the pure-Go
capability kernel**, not the Rust one — the daemon prints `kernel=pure-go-stub`
on boot. The Rust kernel exists in `core/` behind `-tags ffi` and a tiny C-ABI
(opaque `u64` handles, no pointers across the seam), but it is not what you run,
and it's the build that *can't* enforce scope. Windows + Linux are the real
platforms (audio works on both); macOS builds but its audio backend has never
been compiled.

Repo: https://github.com/hash066/Cerberus — QUICKSTART.md is the two-machine
walkthrough; the README table is the authority on what's Real/Partial/Stub, and
ARCHITECTURE.md §8 now shows the design verdict and what's actually in the tree
side by side, because those had drifted apart. I'd genuinely value adversarial
reads of `daemon/auth` (capability envelope, attenuation, revocation),
`daemon/mesh/capscope.go` (the scope gate that just closed a real hole — if it's
still wrong, that's the finding I most want), and `daemon/wasm` (sandbox) more
than stars.

---

## Anticipated hard questions — answers to have ready

Post these as replies, not preemptively. First-person, concede what's true.

### 1. "Why not just SSH / Tailscale + a script?"

For "run a command on my other box," SSH wins — it's battle-tested and I use it
daily. Cerberus is for the thing SSH is bad at: **partial, revocable, scoped**
access. An SSH key is ambient authority — whoever holds it is you, on that
whole machine. A Cerberus grant is "this GPU device, 16 MiB per dispatch,
exec-only, expires in an hour," and I can revoke it mesh-wide after handing it
out. The other half is heterogeneity: the unit of work is a WASM module, so the
same bytes run on my ARM Mac and x86 tower, sandboxed, without me
cross-compiling or trusting the sender. Tailscale is complementary, not
competing — QUICKSTART documents using it as the underlay across networks.

### 2. "How is this different from exo?"

Different layer, and on exo's own turf exo wins — model-parallel LLM serving is
its whole product and it's mature at it. We only just wired llama.cpp at all
(`-llama-model`, single-node), splitting a model across machines **isn't wired
end-to-end**, and our own measurement says it'd be ~9× slower than not splitting
it. So we're not competing on inference and I'd be lying if I said otherwise. I
haven't benchmarked exo and won't quote numbers at it.

Cerberus pools **the machines** — sandboxed compute, GPU kernels, files, audio —
behind an object-capability model, because the problem I wanted solved was "how
do I let other people's code and my own agents use my hardware without giving
them my machine." If you want 70B inference across your Macs tonight: exo. If
you want a securable machine-mesh substrate: that's this, and it's early.

### 3. "Is the crypto real, or 'crypto'?"

Real, boring cryptography; no tokens, no chain-in-production. Capability
envelopes are Ed25519-signed, attenuation is enforced (child rights must be a
subset, caveats only tighten), revocations are durable and gossiped; the data
plane is QUIC with mutual TLS pinned to the peer's identity; the issuer key
lives in the OS keychain where available (Credential Manager / Keychain /
Secret Service). What it is **not**: audited or formally verified — the trust
bootstrap is "the issuer is the peer the transport authenticated" (self-issuer
model) rather than full CapTP, and I'd love qualified eyes on exactly that
seam. The "economy" is deliberately notional: a durable ledger that logs usage
and a working optimistic fraud-proof challenge, with value transfer switched
off. zk-WASM proof-of-inference is a documented stub (~100× overhead is the
honest reason), not a roadmap bullet pretending to be a feature.

### 4. "9P? In 2026? Why not gRPC like everyone else?"

9P is only the **control plane**, and that's exactly why it fits: a resource is
a path (`/cer/dev/gpu/<peer>/0`), and walk/open are single choke points where
every access gets a capability check — "everything is a file" degenerates
naturally into "every open is an authorization decision." The protocol is small
enough to reason about, which I can't say for a mesh of ad-hoc RPC endpoints.
Bulk bytes (tensors, file shards, audio frames) never ride it — opening a
device `ctl` returns a QUIC data-plane grant instead of data.

On mounting it: `cerberusd -mount /mnt/cerberus` is a **real FUSE mount on
Linux** (pure-Go go-fuse straight to `/dev/fuse` — no cgo, no libfuse, nothing
to install), and every callback re-enters the same capability-checked walk/open
the network path uses, so the mount is a second transport and not a second,
unguarded door. The Windows drive-letter path exists in code (cgofuse's nocgo
binding against WinFsp) but I have **not verified it** — it needs a kernel
driver I haven't tested against, so I'm not claiming it. macOS I deliberately
didn't ship: macFUSE wants a kext and a reboot.

The honest limit: **`/cer/fs` is not browsable through a mount.** Its files
aren't enumerated, and a read-open over 9P returns `ENOSYS` (`wire.go:256`),
because file bytes ride the data plane by design. "Drag a file out of Explorer"
is not a thing Cerberus does; `cerberus fs get` is. (There's also a gRPC-shaped
local RPC for the CLI; 9P is the mesh-facing resource namespace.)

### 5. "What's the actual threat model? What does a malicious peer get?"

A malicious *guest* (workload) gets almost nothing: v0.1 modules can't import
host functions at all — no fs, no net, no clocks — plus a hard execution
timeout; the residual risks are compute-DoS and whatever wazero bugs exist,
which is why there's an adversarial sandbox suite and fuzzing in CI. A
malicious *peer daemon* is the sharper question: it can refuse work, lie about
results (general proof-of-compute is unsolved here — that's precisely the
zk-WASM stub; today you get content-addressed tasks (CIDs), the signed
capability chain saying who was *allowed* to run what, and
recompute-and-challenge in the economy path), and DoS you.
It **cannot** touch your resources without presenting a capability you minted,
and everything it does is bounded by that token's rights/quota/expiry, which
you can revoke. Also worth conceding plainly: the daemon itself runs as you on
your machine — Cerberus sandboxes *workloads*, it does not (yet — TEE is a
stub) protect a workload from a hostile host.

### Bonus jabs to expect

- **"'Hypervisor' is a stretch."** Fair. It doesn't virtualize an OS; it
  virtualizes *resources* (compute/GPU/fs/audio) behind capability handles —
  "distributed hypervisor" in the exokernel sense, not ESXi. If the room
  hates the word, the architecture doc's phrasing is "capability-secured
  distributed resource mesh."
- **"Unsigned installers, seriously?"** Beta economics; signing certs are
  bought, not earned. It's called out in the README, TESTERS.md, and the
  release notes rather than hidden.
- **"Windows-first is a weird flex for infra."** It's where the idle GPUs and
  the gaming rigs are, it's the platform other meshes treat as an
  afterthought — and it forced the capability model to survive an OS with no
  Unix sockets from day one.
- **"Go AND Rust?"** One boundary, deliberately tiny: Go owns the control
  plane, Rust owns the capability kernel/runtime, they meet at a C-ABI that
  passes opaque `u64` handles — no pointers across the seam.

---

## Claims → code (keep this handy while answering)

| Claim | Path |
|---|---|
| Signed capability envelope, attenuation, revocation | `daemon/auth/` (+ `core/ocap/`) |
| Revocation gossip mesh-wide | `daemon/auth/` (revocation gossip), wired in `cmd/cerberusd/main.go` |
| Remote WASM exec, CID-addressed, signed-cap-gated | `cmd/cerberusd/rpc.go` (`Run`), `daemon/compute/`, `daemon/mesh/compute.go` |
| No-import sandbox + timeout | `daemon/wasm/wasm.go` (plain `Instantiate`, `DefaultTimeout`), `daemon/wasm/sandbox_test.go` |
| Cross-node GPU dispatch, honest backend string | `cmd/cerberusd/rpc.go` (`GpuDispatch`), `daemon/mesh/gpu.go`, `core/cabi` (`cerberus_gpu_backend`) |
| 9P namespace, cap-checked walk/open, ctl→data-plane grant | `daemon/ninep/`, `daemon/system/system.go` |
| QUIC data plane, mTLS + PeerID pinning | `daemon/dataplane/` |
| Erasure-coded distributed FS, peer scatter | `daemon/dfs/`, `daemon/system/` (`peerscatter_test.go`) |
| Cross-node audio (Windows WASAPI + Linux PulseAudio), direction-scoped session caps | `daemon/audio/os_windows.go`, `os_linux.go`, `daemon/audiolink/` |
| macOS audio written but NEVER COMPILED | `daemon/audio/os_darwin.go` (`//go:build darwin && cgo && cerberus_coreaudio`); default macOS builds get `os_darwin_stub.go` |
| Pipeline across nodes, activations over data plane | `daemon/system/pipeline.go`, `test/pipeline_e2e/` |
| Mocks DELETED, not repaired; removed backends fail loudly | `daemon/inference/backend.go:54`, `inference_test.go:34` (pins the deletion) |
| Real llama.cpp chat backend, opt-in via `-llama-model` | `cmd/cerberusd/main.go:509` (`llama.NewService` → `gw.SetInference`), `daemon/llama/` |
| Cap-gated ggml-rpc worker, OFF by default, with an honest flag doc | `-llama-worker` (`cmd/cerberusd/main.go:130`), `daemon/llama/worker.go`, `daemon/mesh/llamarpc.go` |
| Distributed offload NOT wired: forwarder constructed by nobody | `daemon/llama/forwarder.go` — `OpenLlamaRPCSession` called only there; no caller of `NewForwarder`. Worker's `ggml-rpc-server` is loopback-only (`rpcserver.go:27`); `-llama-rpc` is a raw dial past the gate |
| Capabilities scoped to their resource (shipping kernel) | `contract/go/stub/stub.go:113`, `daemon/ninep/capscope_proof_test.go` |
| …and at every mesh gate (this closed a real hole) | `daemon/mesh/capscope.go` (`grantCoversResource`), `daemon/mesh/capscope_proof_test.go` (per-protocol denial + prefix/wildcard rejection) |
| …and the `-tags ffi` kernel structurally can't | `daemon/ffi/kernel_ffi.go:170` — C ABI is `(handle, op, now)`, no resource |
| VRAM measured, never fabricated; unknown stays unknown | `daemon/gpu/vramprobe_nvidia.go`, [docs/vram.md](../vram.md) |
| Real Linux FUSE mount (pure Go, no cgo) | `daemon/ninep/mount_linux.go`, `mount_linux_test.go` (`TestMountLive`) |
| `/cer/fs` NOT readable through a mount | `daemon/ninep/wire.go:256` (read-open → `ENOSYS`) |
| A test that forbids claiming RDMA | `daemon/hostinfo/hostinfo_test.go:63` |
| Gateway answers for models it doesn't have (known bug) | `daemon/gateway/gateway.go:160` (`componentFor` falls through), `chat.go:186` (fabricated `usage`) |
| Economy = usage ledger + fraud-proof challenge, no value transfer | `daemon/ledger/`, `daemon/economy/`, `cerberus economy challenge` |
| MCP server (12 tools) | `cmd/cerberus-mcp/`, `docs/mcp.md` |
| Frontier stubs labelled | `ARCHITECTURE.md` §8, `CLAUDE.md` ("maturity honesty") |

## Logistics (see docs/launch/CHECKLIST.md)

- Post a weekday, 7–9am Pacific. Stay at the keyboard for the first 3 hours.
- Reply to every substantive technical comment; concede fast, link code.
- Do not ask anyone to upvote; do not reply to every flame; never edit the
  post to argue with the thread.
