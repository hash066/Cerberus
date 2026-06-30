# Project Cerberus — Volume II: The Sovereign Compute Mesh

> **Status:** Architecture synthesis / idea-dump. The canonical, build-grade specification lives in [ARCHITECTURE.md](ARCHITECTURE.md); the low-level per-vertical designs live under [docs/verticals/](docs/verticals/). This document is the *narrative* — it records the debate, the decisions, and the reasoning. Volume I ([README.md](README.md)) defined the P2P hyper-computer; Volume II hardens it into a **zero-trust distributed hypervisor for multi-agent orchestration**.

---

## 1. The Structural-Failure Thesis

Cerberus Volume II rejects the generic-SaaS framing entirely. It exists to address a *structural* failure mode rather than a convenience gap.

The structural failure is **centralized cloud monopoly bottlenecking continuous, autonomous AI agent swarms.** Modern agentic systems do not run a request and stop; they run *continuously*, spawning sub-agents, holding long-lived memory, and consuming compute in bursty, unpredictable, parallel waves. Renting that pattern from AWS/GCP is economically ruinous (idle-billed GPUs, egress taxes) and architecturally fragile (a single provider's control plane is the single point of failure for an entire autonomous fleet).

Cerberus treats every machine an organization already owns — Apple Silicon laptops, Windows gaming rigs, headless Linux boxes — as a **fluid pool of capabilities**: execution threads, VRAM pages, block storage, and peripheral I/O. It binds them into a local, zero-configuration, **capability-secured** mesh on which agent swarms can run, remember, and settle accounts without a central master and without a cloud bill.

> **Explicit non-goal:** Cerberus is not a generic compute SaaS, not a Kubernetes replacement for stateless web services, and not a crypto product. It serves *non-normal, decentralized, agentic* systems.

---

## 2. The Architectural Debate — Synthesis Record

Volume II was forged by forcing five elite-archetype agents to fight over the stack until a single coherent Go/Rust architecture survived. The debate is recorded here as a **decision ledger** (third-person), not a transcript. Each row: who demanded what, how it resolved, the trade-off accepted, and an honest maturity verdict.

| # | Agent | Core demand | Resolution | Trade-off | Verdict |
|---|-------|-------------|------------|-----------|---------|
| 1 | **The Kernel Hacker** | Plan 9 / 9P to mount remote GPUs & VRAM as local files via FUSE | 9P2000.L over QUIC becomes the **control-plane namespace** (`/cer/dev/gpu`, `/cer/dev/vram`); the **data plane is separate** — QUIC zero-copy + RDMA-over-Thunderbolt | Romance of "VRAM is a file you `read()`" is abandoned; 9P enumerates and *grants*, it does not stream tensors | 9P-control **Shippable**; RDMA-over-TB **Shippable on Mac**, Frontier elsewhere |
| 2 | **The Security Architect** | Object-Capability (OCap, Spritely Goblins) on the WASM Component Model | OCap becomes the **spine of the whole system**: Component-Model typed resource handles *are* unforgeable capabilities; no ACLs, no identity, no ambient authority | Capability plumbing pervades every call path; key-custody surface grows | OCap-on-Component-Model **Shippable & best-in-class**; full distributed CapTP ambitious |
| 3 | **The Network/State Engineer** | libp2p topology + CRDTs + Erlang/OTP supervision trees | **Hybrid fabric:** Zenoh for the intra-site data-centric layer (telemetry, CRDT deltas), libp2p (DCUtR + Gossipsub v1.1) only to cross sites/NAT. CRDTs + vector clocks for memory; OTP-style supervision trees for daemon fault tolerance | The directive's "libp2p everywhere" loses to the project's own field evidence (see §3) | **Shippable** |
| 4 | **The AI Orchestrator** | Dynamic tensor sharding, WASM execution, promise pipelining to mask latency | Pipeline + tensor parallelism over QUIC; CapTP-style **promise pipelining** to hide Wi-Fi round-trips; workers are WASM **components**, accelerated by MLX / wgpu+tinygrad | Real-time inference of very large models over Wi-Fi remains latency-bound | Execution + sharding **Shippable**; real-time 70B-over-Wi-Fi **Frontier** |
| 5 | **The Cryptoeconomist** | Cross-chain wallets in agents, eUTXO micro-settlement, zk-WASM anti-cheat, hardware kill-switches | The entire economy is quarantined into an **optional, profile-gated "Open Mesh" tier** (§4). IPLD delivers model weights peer-to-peer regardless of profile | On-chain settlement is *off* for regulated buyers; zk-WASM proof-of-inference is acknowledged as a 100×-overhead research bet | IPLD **Shippable**; eUTXO **Buildable**; zk-WASM-inference **Frontier** |

### The one debate the project had already settled against the directive

The directive *demanded* libp2p. But [braindomp.md](braindomp.md) records the hard-won field lesson from `exo`: **libp2p's Kademlia DHT melted under messy local Wi-Fi**, producing latency spikes when nodes tried to sync global cluster state — so exo ripped it out for **Zenoh**. Volume II honors the evidence over the dogma: Zenoh owns the intra-site fabric (4-byte overhead, data-centric pub/sub, automatic mesh healing), and libp2p is retained *only* for what it is genuinely best at — NAT traversal and relay-upgrade (DCUtR) across sites. This is the single most important course-correction in the volume.

---

## 3. Two Profiles, One Codebase

The deepest tension in the directive — "trustless crypto economy" versus "ambient medical prediction and infrastructure grids" — is resolved structurally. Cerberus ships **one binary with two runtime profiles**:

```
                         ┌───────────────────────────────────────────┐
                         │             CERBERUS CORE                  │
                         │   (OCap spine · mesh · CRDTs · 9P · WASM)  │
                         └───────────────┬───────────────────────────┘
                                         │  profile switch (boot-time)
                 ┌───────────────────────┴───────────────────────────┐
                 ▼                                                     ▼
   ┌──────────────────────────────┐                  ┌──────────────────────────────┐
   │        OPEN MESH             │                  │           SEALED              │
   │  (beachhead / dev / startup) │                  │  (enterprise / infra / med)   │
   ├──────────────────────────────┤                  ├──────────────────────────────┤
   │ eUTXO economy        ON      │                  │ chain / wallets       OFF     │
   │ zk-WASM anti-cheat   ON      │                  │ TEE attestation       ON      │
   │ cross-org compute    ON      │                  │ compliance / audit    ON      │
   │ kill-switch (on-chain)       │                  │ kill-switch (governed)        │
   └──────────────────────────────┘                  └──────────────────────────────┘
```

- **Open Mesh** is the beachhead profile: the eUTXO micro-economy is *on*, because the economy is precisely the incentive that lets independent startups pool and trade compute trustlessly. zk-WASM proofs police cheating.
- **Sealed** is the enterprise-expansion profile: the chain is *off*, TEE attestation and audit are *on*, and the kill-switch becomes a governed administrative capability rather than an on-chain contract.

The point: the crypto layer is *not* load-bearing for the regulated verticals. The same mesh, scheduler, security kernel, and peripheral fabric serve both.

---

## 4. The Unified Stack (overview)

Authoritative detail is in [ARCHITECTURE.md](ARCHITECTURE.md); this is the orientation map.

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  SURFACE        cerberusd (Go daemon) · cerberus (CLI) · Tray UI (Tauri v2)    │ ← no browser
├──────────────────────────────────────────────────────────────────────────────┤
│  ECONOMY*       eUTXO settlement · agent wallets · zk-WASM · IPLD weights       │ *Open Mesh only
├──────────────────────────────────────────────────────────────────────────────┤
│  ORCHESTRATION  Placement/Scheduling brain · tensor+pipeline sharding · promises │
├──────────────────────────────────────────────────────────────────────────────┤
│  EXECUTION      WASM Component Model (Wasmtime/Cranelift) · MLX · wgpu · tinygrad│
├──────────────────────────────────────────────────────────────────────────────┤
│  STATE          CRDTs (Automerge/yrs) + vector clocks · per-domain reducers     │
├──────────────────────────────────────────────────────────────────────────────┤
│  PERIPHERAL     9P2000.L control namespace · FUSE/WinFsp · IPLD FS · AES67/ROC   │
├──────────────────────────────────────────────────────────────────────────────┤
│  TRANSPORT      Zenoh (intra-site) + libp2p DCUtR/Gossipsub (inter-site) / QUIC  │
├──────────────────────────────────────────────────────────────────────────────┤
│  ███████████  OCAP SECURITY KERNEL — the spine under everything  ███████████     │
└──────────────────────────────────────────────────────────────────────────────┘
   Language split:  Go = orchestration/daemon/control-plane   ·   Rust = capabilities/runtime/crypto
```

| Layer | Choice | Language |
|---|---|---|
| Daemon, CLI, supervision, scheduler | Go 1.22+ | Go |
| Capability kernel, WASM runtime, crypto, CRDT engine | Rust | Rust |
| Intra-site fabric | Zenoh | Go (binding) |
| Inter-site fabric | libp2p (DCUtR, Gossipsub v1.1) over QUIC | Go |
| Sandboxed execution | Wasmtime + WASM Component Model + WASI P2 | Rust |
| AI acceleration | MLX (Apple) · wgpu + tinygrad (PC/Linux) | Rust/native |
| Distributed memory | Automerge / `yrs` + vector clocks | Rust ↔ Go FFI |
| Peripheral control plane | 9P2000.L; `hanwen/go-fuse`; WinFsp + cgofuse | Go |
| Content-addressed storage / weight delivery | IPLD + Reed-Solomon (JuiceFS-style split) | Go |
| Audio virtualization | PipeWire/CoreAudio capture → AES67/ROC over QUIC | Go/native |
| Settlement (Open Mesh) | eUTXO chain · zk-WASM (Delphinus-style) | Rust |
| Desktop UI | Tauri v2, tray-first, native webview | Rust + minimal JS |

---

## 5. Tour of the Verticals

Ten verticals plus the spine. Each links to its low-level design doc. Six are reframed from Volume I + the directive; **four are net-new** (surfaced during the debate as missing).

| # | Vertical | Owner archetype | One-line essence | Doc |
|---|----------|-----------------|------------------|-----|
| 00 | **OCap Security Kernel** *(spine)* | Security Architect | Unforgeable capability handles on the WASM Component Model; no ambient authority | [00](docs/verticals/00-ocap-security-kernel.md) |
| 01 | **Mesh Fabric & Transport** | Network/State Eng. | Zenoh intra-site + libp2p inter-site over QUIC; OTP supervision | [01](docs/verticals/01-mesh-fabric-transport.md) |
| 02 | **Distributed State & Memory (CRDTs)** | Network/State Eng. | Conflict-free agent memory; vector clocks; contradictions flagged, never silently merged | [02](docs/verticals/02-distributed-state-crdts.md) |
| 03 | **Compute Orchestration** | AI Orchestrator | Tensor/pipeline sharding + WASM workers + promise pipelining | [03](docs/verticals/03-compute-orchestration.md) |
| 04 | **9P Peripheral Virtualization** | Kernel Hacker | File-as-capability device namespace; data plane stays off 9P | [04](docs/verticals/04-9p-peripheral-virt.md) |
| 05 | **eUTXO & Open Mesh Economy** *(optional)* | Cryptoeconomist | Trustless off-chain settlement; zk-WASM anti-cheat; IPLD weights | [05](docs/verticals/05-eutxo-open-mesh-economy.md) |
| 06 | **Placement & Scheduling Brain** *(NEW)* | — | The actual hard problem: who runs what, where, when | [06](docs/verticals/06-placement-scheduling.md) |
| 07 | **Identity & Capability Lifecycle** *(NEW)* | — | Mint / attenuate / delegate / revoke; root-of-trust bootstrap | [07](docs/verticals/07-identity-cap-lifecycle.md) |
| 08 | **Observability & Tracing** *(NEW)* | — | Distributed traces of workflows, capability flows, proofs | [08](docs/verticals/08-observability-tracing.md) |
| 09 | **Power, Thermal & Sleep Lifecycle** *(NEW)* | — | Desktops close their lids; lifecycle is a first-class scheduler input | [09](docs/verticals/09-power-thermal-sleep.md) |
| 10 | **Desktop App Model** | — | Headless daemon + CLI + tray UI; browsers forbidden | [10](docs/verticals/10-desktop-app-model.md) |

### Why the four new verticals were unavoidable
- **Scheduling brain (06):** the directive listed *what* to pool but never *who decides placement*. That decision — a multi-objective constraint solve over latency, thermal headroom, VRAM, and capability availability — is the single hardest problem in the system. Leaving it implicit would have been malpractice.
- **Identity & capability lifecycle (07):** OCap is only as good as its issuance and revocation story. "Who mints the first capability, and how is one revoked across a partitioned mesh?" needed its own spec.
- **Observability (08):** a zero-trust mesh where you cannot trace a capability's flow or verify a compute proof is unauditable — fatal for the Sealed profile.
- **Power/thermal/sleep (09):** the target nodes are *laptops*. They sleep, throttle, and have their lids closed mid-pipeline. Treating that as an exception rather than a first-class lifecycle input was the most common way Volume I's assumptions would have shattered in the field.

---

## 6. Go-To-Market

### Beachhead
High-burn, agentic-AI startups drowning in cloud bills for continuous agent swarms. Cerberus lets them pool the Macs and PCs already on their desks into a local, zero-trust mesh — escaping cloud-monopoly economics without surrendering security. Adoption is **developer-led**: the friction to bind three office laptops into a mesh is near zero, and that grassroots adoption bootstraps both the **Open Mesh eUTXO economy** (cross-org compute trading) and the **OCap trust graph**.

### Monetization

```
 OPEN-CORE  ──────────────────────────────────────────────────────────────────►
 │
 ├─ FREE:        the mesh, OCap kernel, CRDT memory, 9P fabric, WASM execution
 │
 ├─ PROTOCOL TAX (Open Mesh):  a small protocol-level skim on every eUTXO compute
 │                             settlement. The mesh is free; the trustless
 │                             *settlement rail* is the revenue. Scales with the
 │                             cross-org compute economy, not with seats.
 │
 └─ ZERO-TRUST ENTERPRISE TIERS (Sealed):  TEE attestation, compliance packs
                                (HIPAA/MDR/IEC-62443 for the medical & infra-grid
                                expansion), SLAs, governed kill-switch, audit
                                export, priority support. Per-fleet licensing.
```

- **The Protocol Tax** monetizes the Open Mesh economy itself — usage-proportional, not seat-based — so revenue tracks the actual compute economy the network creates.
- **Zero-Trust Enterprise Tiers** monetize the Sealed profile for the regulated expansion verticals (autonomous infrastructure grids, ambient medical prediction), where buyers pay for attestation, compliance, and governance rather than for the compute itself.

### Funnel
`dev pools 3 laptops (free)` → `team adopts Open Mesh + pays Protocol Tax on cross-org compute` → `org graduates to Sealed enterprise tier for regulated deployment`.

---

## 7. Required Literature & Specifications

Extends [extra.md](extra.md) with the Volume II paradigms.

- **Plan 9 / 9P:** [9P2000 protocol](http://9p.cat-v.org/) · [styx/9P design](https://9p.io/sys/doc/9.html)
- **Object Capabilities:** [Spritely Goblins](https://spritely.institute/goblins/) · [CapTP](https://github.com/ocapn/ocapn) · [Capability Myths Demolished](https://srl.cs.jhu.edu/pubs/SRL2003-02.pdf)
- **WASM Component Model:** [component-model spec](https://github.com/WebAssembly/component-model) · [WASI Preview 2](https://github.com/WebAssembly/WASI) · [Wasmtime](https://wasmtime.dev/)
- **CRDTs & causality:** [Automerge](https://automerge.org/) · [Yjs/`yrs`](https://github.com/y-crdt/y-crdt) · [Shapiro et al., CRDTs](https://hal.inria.fr/inria-00609399/document) · [Lamport vector clocks](https://lamport.azurewebsites.net/pubs/time-clocks.pdf)
- **Transport:** [Zenoh](https://zenoh.io/) · [libp2p DCUtR](https://docs.libp2p.io/concepts/nat/dcutr/) · [Gossipsub v1.1](https://github.com/libp2p/specs/blob/master/pubsub/gossipsub/gossipsub-v1.1.md)
- **Settlement & proofs:** [Cardano EUTXO](https://iohk.io/en/research/library/papers/the-extended-utxo-model/) · [Delphinus zkWasm](https://github.com/DelphinusLab/zkWasm)
- **Content addressing:** [IPLD](https://ipld.io/) · [Reed-Solomon erasure](https://github.com/klauspost/reedsolomon)
- **Trusted execution:** [Intel TDX](https://www.intel.com/content/www/us/en/developer/tools/trust-domain-extensions/overview.html) · [AMD SEV-SNP](https://www.amd.com/en/developer/sev.html) · [Apple Secure Enclave](https://support.apple.com/guide/security/secure-enclave-sec59b0b31ff/web)
- **Fault tolerance:** [Erlang/OTP supervision principles](https://www.erlang.org/doc/design_principles/sup_princ.html)
- **Peripheral & storage (from Volume I):** [JuiceFS](https://github.com/juicedata/juicefs) · [hanwen/go-fuse](https://github.com/hanwen/go-fuse) · [WinFsp](https://github.com/winfsp/winfsp) · [ROC Toolkit / AES67](https://roc-streaming.org/)

---

*Volume II is the synthesis. Build to [ARCHITECTURE.md](ARCHITECTURE.md).*
