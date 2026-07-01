# Cerberus User Guide

The concepts behind the commands — capabilities, the two profiles, pooling your
devices' peripherals, the desktop app, and the compute economy — in plain terms,
honest about what's real today and what's next.

If you just want to *run* something, start with
[docs/getting-started.md](getting-started.md). This guide explains the *why*.

- [What Cerberus is](#what-cerberus-is)
- [Capabilities in plain terms](#capabilities-in-plain-terms) (the security model)
- [Two profiles: open_mesh vs sealed](#two-profiles-open_mesh-vs-sealed)
- [Pooling peripherals](#pooling-peripherals-gpu-vram-audio-storage)
- [The desktop app](#the-desktop-app)
- [The economy (open_mesh)](#the-economy-open_mesh)
- [What's next / Frontier](#whats-next--frontier)

---

## What Cerberus is

Cerberus turns the heterogeneous machines you already own — Macs, PCs, Linux
boxes, laptops — into **one private mesh** that runs sandboxed workloads and (in
the fullness of the vision) shares peripherals and remembers across nodes. It is:

- **Masterless.** No coordinator, no cloud account. Any node can drop without
  killing the mesh. Nodes find each other on the LAN with zero configuration.
- **Zero-trust.** Nothing acts on identity or role. Every cross-boundary action
  presents an unforgeable **capability** — a key to a specific resource that you
  can narrow, delegate, and revoke.
- **Heterogeneous by default.** Portability comes from compiling workloads to
  **WebAssembly** components, not from shipping native binaries across
  architectures. An ARM Mac and an x86 PC run the same shard.
- **Two profiles, one binary.** `open_mesh` (compute economy on) and `sealed`
  (economy off, attestation on) are a boot-time switch, not a fork.

The canonical spec is [ARCHITECTURE.md](../ARCHITECTURE.md); the full status and
roadmap is [VISION-AND-ROADMAP.md](../VISION-AND-ROADMAP.md).

---

## Capabilities in plain terms

Cerberus's spine is **object-capability (OCap) security**. Forget usernames,
passwords, and roles. The only way to do anything is to **hold a capability** for
it — an unforgeable, cryptographically signed token that says *"the holder may do
X to resource Y, under these limits, until this time."*

Four properties make this powerful:

1. **Unforgeable.** A token is Ed25519-signed by the daemon's issuer key. You
   can't fabricate or tamper with one — verification catches it.
2. **Attenuable.** You can derive a *strictly narrower* child token from one you
   hold: drop rights, tighten the resource scope, shorten the lifetime. A child
   can never exceed its parent. This is how you delegate safely — hand a tool a
   token that can *only* do the one thing it needs.
3. **Revocable.** Revoke a token by id and it stops verifying. Revocations are
   **durable** (survive restart) and **gossip across the mesh** — revoke on node A
   and node B denies it too.
4. **Time-bounded.** Tokens carry a not-before and an expiry. The operator token,
   for example, is valid for 24 hours.

### How you experience this

- On startup the daemon mints the **operator token** (subject `operator`, `admin`
  rights) and writes it to your OS config dir. The CLI reads it; you export it as
  `$CERBERUS_TOKEN` for curl/Python. See
  [getting-started §4](getting-started.md#4-the-operator-token).
- Every network surface enforces a specific **right**:

  | Surface | Right required |
  |---|---|
  | Gateway `POST /v1/chat/completions` (`:8080`) | `exec` |
  | Status API `GET /api/v1/status` (`:7777`) | `read` |
  | Metrics `GET /metrics` (`:7779`) | `read` |
  | RPC / `cerberus status` (`:9092`) | `read` |

  `admin` satisfies any right, which is why the operator token works everywhere.
  Health probes (`/healthz`, `/readyz`) are intentionally open for orchestrators.

- **Least privilege in practice:** instead of handing a tool your `admin` operator
  token, derive a narrow one. Conceptually: take the parent, *keep only* `exec`,
  optionally pin it to a resource path, give it a short TTL — then set that as the
  tool's `CERBERUS_TOKEN`. If it leaks, it can do far less, and you can revoke it.

> **Under the hood.** The Go daemon's default build uses a pure-Go Ed25519 token
> issuer. A real Rust capability kernel (CBOR caps + attenuation chains, the
> `SignedKernel`) can be bound in via cgo under `-tags ffi` so capabilities are
> enforced by the same crypto core across the FFI boundary — see
> [docs/ffi.md](ffi.md). Either way, the model you experience is identical.

**No ambient authority** is the rule with no exceptions: there is no
unauthenticated path, no "trusted localhost bypass" for data endpoints, no role
you can assume. If code doesn't hold the key, it can't act.

---

## Two profiles: open_mesh vs sealed

Cerberus ships one binary with two operating modes, chosen at boot with
`cerberusd -profile=<open_mesh|sealed>` (default `open_mesh`).

| Feature | `open_mesh` (default) | `sealed` |
|---|---|---|
| Compute economy / credit wallets | **ON** — daemon mints genesis credits | **OFF** (boot-rejected if enabled) |
| zk-WASM / optimistic settlement | ON | OFF |
| Cross-org compute trading | ON | intra-org only |
| TEE attestation required | optional | **ON** |
| Audit export / retention | optional | **ON** |
| Kill-switch | on-chain capability | governed admin capability |
| Intended for | startups, personal/dev meshes | infra grids, medical, regulated |

**In practice today:**
- Under `open_mesh`, the daemon opens the durable credit ledger and, if the
  operator has no balance, mints genesis compute credits (you'll see
  `operator balance=1000000` in the log and `operator_balance` in the status API).
- Under `sealed`, the economy is off; attestation and audit are the emphasis. The
  attestation/TEE enforcement pieces are partly Frontier (see below) — the profile
  switch itself is real and boot-validated.

Pick `sealed` when you don't want an economy in the loop and want the
attestation-first posture; pick `open_mesh` for a personal/dev mesh.

---

## Pooling peripherals (GPU, VRAM, audio, storage)

The long-term promise is that a remote machine's GPU/VRAM, microphone, speakers,
or disk appear as if plugged into the machine in front of you — addressed as a
**capability-gated file namespace** (9P), with the actual bytes flowing over a
**separate fast data plane** (QUIC), never over the control plane.

Here's the honest state of each, so you know what you can lean on:

| Peripheral | Transport / namespace | OS/hardware edge | Status |
|---|---|---|---|
| **Storage / files** | 9P2000.L namespace + QUIC data plane; distributed FS with IPLD chunking + Reed-Solomon erasure coding (`daemon/dfs`) | mounting as a local drive needs **FUSE/WinFsp** | Transport + erasure coding **real & tested**; the **mount** is a labelled stub |
| **GPU / VRAM** | capability + byte-quota-bound QUIC transfer; a `gpu` dispatch abstraction with a software backend and a real **wgpu** backend | needs a **real GPU** to validate honestly (the wgpu backend has run on the dev box's NVIDIA GPU) | Data plane **real**; GPU dispatch **off by default / hardware-gated** |
| **Audio (mic/speaker)** | packetized network audio: jitter buffer, clock-drift (DLL) control, gap-fill; rides one capability/quota-bound QUIC transfer | **OS capture/playback** (CoreAudio/WASAPI/PipeWire) | Network **transport real & tested**; the **OS capture source** is a labelled stub |

**The pattern is consistent and real:** the **control plane** (9P namespace) mints
a capability + byte-quota **grant** and hands back a dialable endpoint; the **data
plane** (QUIC) moves the bytes under that grant, rejecting over-quota transfers up
front and mid-stream. What's stubbed is always the *last mile into the OS/hardware*
(a kernel mount driver, a real GPU, an OS audio device) — those are honestly
labelled, never faked.

For example, opening a device's `.../ctl` in the 9P namespace calls
`dataplane.RegisterGrant` and returns the live endpoint — proven end-to-end in the
daemon's tests (an over-quota transfer is rejected).

---

## The desktop app

Cerberus is designed as a **tray-first desktop app**: the headless daemon
(`cerberusd`) plus the CLI (`cerberus`) plus a **Tauri v2** system-tray dashboard.

- The dashboard is a native Tauri app: the **Rust shell performs the authenticated
  fetch** against the status API (`127.0.0.1:7777/api/v1/status`, `read`-gated) and
  the native WebView renders it. It is **not** a browser page — opening the raw
  HTML in a browser looks unstyled because the Tauri runtime and its auth aren't
  loaded there.
- It shows live state: profile, kernel, uptime, mesh up/down and peers, operator
  credit balance, and power/lid/battery state.

> **Status: real code, not verified headlessly.** The Tauri app is real and reads
> live data, but launching and visually verifying the GUI needs the **Tauri
> toolchain + WebView2** and a real desktop session — which hasn't been done in the
> headless dev environment. Treat "it renders and feels good" as unverified until
> validated on real hardware. To try it: run `cmd/cerberusd`, then
> `cd tray && cargo tauri dev`.

Everything the GUI shows is also available headlessly via `cerberus status`, the
status API JSON, and the `/metrics` endpoint — so you are never blocked on the GUI.

---

## The economy (open_mesh)

In `open_mesh`, idle compute can be accounted for and (eventually) traded across
organisations with cryptographic settlement — so a provider can't bill for work it
didn't do.

What's real today:
- A **durable eUTXO credit ledger** (`daemon/ledger`, bbolt-backed) that survives
  restart. Under `open_mesh` the operator is granted genesis credits; the balance
  is visible in the status API (`operator_balance`).
- **eUTXO optimistic settlement + fraud-proof slashing** exists and is durable /
  restart-safe (`core/economy`, `daemon/economy`).

What's still model-only / next:
- **Live cross-org settlement wired to compute completion** (settle a real trade in
  credits when a job finishes) is the integration lead step.
- **zk-WASM proof-of-inference** settlement is Frontier (see below).

Under `sealed`, the economy is off entirely (and boot-rejected if you try to
enable it) — that's the mode for regulated/infra deployments where trading isn't
wanted.

---

## What's next / Frontier

Cerberus is deliberately honest about maturity. Some capabilities are **Frontier**
— research-grade, opt-in, kept off the critical path (they may stay partial due to
cost or hardware limits). These are documented stubs by design, never presented as
working:

- **zk-WASM proof-of-inference** — cryptographic proof that a shard really ran a
  computation (~100× overhead today).
- **Host-TEE memory shielding** — hardware-enforced memory isolation on consumer
  desktops (hardware-limited).
- **RDMA-over-Thunderbolt** — ultra-low-latency data plane (shippable on Mac,
  Frontier elsewhere).

Near-term (not Frontier, actively being wired):
- Real GPU dispatch on GPU hardware; FUSE/WinFsp mounts; OS audio capture — the
  hardware/OS-bound trio.
- Full mTLS + PeerID-pinning on the raw data-plane transport.
- Multi-machine, multi-OS bring-up and signed installers + auto-update.

For the complete phased roadmap, ownership, and honest risk list, see
[VISION-AND-ROADMAP.md](../VISION-AND-ROADMAP.md) and
[LAUNCH-PLAN.md](../LAUNCH-PLAN.md).

---

See also: [docs/getting-started.md](getting-started.md) ·
[docs/cli.md](cli.md) · [docs/gateway.md](gateway.md) ·
[ARCHITECTURE.md](../ARCHITECTURE.md).
