# Cerberus — Master System Architecture (Canonical Specification)

> **Authority:** This is the canonical specification. All vertical designs under [docs/verticals/](docs/verticals/) conform to it. The narrative/rationale lives in [ideadumpp2.md](ideadumpp2.md). Where this document and any vertical doc disagree, **this document wins** and the vertical doc is a bug.
>
> **Audience:** engineers implementing Cerberus. Third-person, normative ("MUST/SHOULD/MAY" per RFC 2119 sense).

---

## 1. System Overview & Design Principles

Cerberus is a **zero-configuration, masterless, capability-secured distributed hypervisor** that binds heterogeneous personal/office hardware into a single mesh on which autonomous agent swarms execute, remember, and (optionally) settle compute economically.

Seven principles govern every decision. They are non-negotiable; a design that violates one is rejected.

1. **No ambient authority.** Nothing acts on identity or role. Every action requires an unforgeable **capability**. (See [00-ocap-security-kernel](docs/verticals/00-ocap-security-kernel.md).)
2. **Control plane ≠ data plane.** Naming, discovery, granting, and scheduling are *control* (9P, Zenoh, small messages). Tensors, VRAM pages, file blocks, audio frames are *data* (QUIC zero-copy / RDMA). The two MUST NOT be conflated.
3. **Masterless.** There is no permanent coordinator. Coordination is emergent (CRDT state) or ephemeral-elected (Raft for short-lived leases). Any node may drop without killing the mesh.
4. **Partition tolerance over consistency.** The mesh assumes chaotic Wi-Fi. State is CRDT-merged on reconnect. Convergence is guaranteed; *correctness* of contradictory merges is escalated to humans, never silently resolved.
5. **Heterogeneity is the default.** ARM/x86, Metal/Vulkan/CUDA, macOS/Windows/Linux. Portability is achieved by compiling to WASM components and abstracting GPUs via wgpu/MLX — never by shipping native binaries across architectures.
6. **Zero configuration.** A node joins by launching the daemon. Discovery, key generation, and capability bootstrap are automatic.
7. **Two profiles, one codebase.** `OpenMesh` (economy on) and `Sealed` (economy off, attestation on) are a boot-time switch, not a fork. (See §7.)

### 1.1 Master Architecture Diagram

```
╔══════════════════════════════════════════════════════════════════════════════════╗
║                                 CERBERUS NODE                                       ║
║                                                                                    ║
║  ┌────────────┐   ┌────────────┐   ┌──────────────────────────────┐                ║
║  │ cerberus   │   │  Tray UI   │   │  external agents / OpenAI-     │                ║
║  │  (CLI)     │   │ (Tauri v2) │   │  compatible gateway            │                ║
║  └─────┬──────┘   └─────┬──────┘   └───────────────┬──────────────┘                ║
║        │  local IPC (UDS / localhost gRPC, cap-gated)                               ║
║        └────────────────┴──────────────────────────┘                               ║
║                              │                                                      ║
║   ┌──────────────────────────▼───────────────────────────────────────────────┐    ║
║   │                    cerberusd  (Go daemon)                                  │    ║
║   │   OTP-style supervision tree                                              │    ║
║   │   ┌───────────┐ ┌───────────┐ ┌───────────┐ ┌───────────┐ ┌───────────┐  │    ║
║   │   │ Scheduler │ │ Telemetry │ │ 9P server │ │ Lifecycle │ │  Gateway  │  │    ║
║   │   │   (06)    │ │   (08)    │ │   (04)    │ │   (09)    │ │  (10)     │  │    ║
║   │   └─────┬─────┘ └─────┬─────┘ └─────┬─────┘ └─────┬─────┘ └───────────┘  │    ║
║   └─────────┼─────────────┼─────────────┼─────────────┼───────────────────────┘    ║
║             │  FFI (cgo / C-ABI) boundary  ▼                                        ║
║   ┌─────────▼─────────────▼─────────────▼─────────────▼───────────────────────┐    ║
║   │                  cerberus-core  (Rust crates)                              │    ║
║   │  ┌──────────────┐ ┌──────────────┐ ┌──────────────┐ ┌──────────────────┐  │    ║
║   │  │  OCap kernel │ │ WASM runtime │ │ CRDT engine  │ │ economy (OpenMesh)│  │    ║
║   │  │   (00)       │ │ Wasmtime(03) │ │  (02)        │ │  eUTXO/zk-WASM(05)│  │    ║
║   │  └──────────────┘ └──────────────┘ └──────────────┘ └──────────────────┘  │    ║
║   └──────────────────────────────┬────────────────────────────────────────────┘    ║
║                                   │                                                 ║
║   ┌───────────────────────────────▼────────────────────────────────────────────┐   ║
║   │  TRANSPORT:  Zenoh (intra-site control)  ·  libp2p DCUtR/Gossipsub (inter)   │   ║
║   │  DATA PLANE: QUIC zero-copy streams  ·  RDMA-over-Thunderbolt (when present)  │   ║
║   └──────────────────────────────────────────────────────────────────────────────┘   ║
╚══════════════════════════════════════════════════════════════════════════════════╝
        ▲ every arrow above carries a CAPABILITY, not an identity ▲
```

---

## 2. Repository / Monorepo Structure

A single monorepo. Go owns the orchestration/control plane; Rust owns the security/runtime/crypto core; they meet at a narrow C-ABI FFI boundary and a WASM component boundary.

```
Cerberus/
├── README.md                 # Volume I (P2P hyper-computer)
├── ideadumpp2.md             # Volume II narrative / debate
├── ARCHITECTURE.md           # ← this file (canonical)
├── extra.md  braindomp.md    # research provenance
│
├── cmd/
│   ├── cerberusd/            # Go: the headless daemon entrypoint
│   └── cerberus/             # Go: the CLI entrypoint
│
├── daemon/                   # Go control plane
│   ├── supervisor/           # OTP-style supervision trees
│   ├── scheduler/            # vertical 06 — placement brain
│   ├── telemetry/            # vertical 08 — metrics + tracing
│   ├── ninep/                # vertical 04 — 9P2000.L server + FUSE/WinFsp bridge
│   ├── lifecycle/            # vertical 09 — power/thermal/sleep
│   ├── mesh/                 # vertical 01 — Zenoh + libp2p fabric
│   ├── gateway/              # vertical 10 — OpenAI-compatible gateway + IPC
│   └── ffi/                  # cgo bindings to cerberus-core
│
├── core/                     # Rust workspace (cerberus-core)
│   ├── ocap/                 # vertical 00 — capability kernel + CapTP
│   ├── runtime/              # vertical 03 — Wasmtime + component model host
│   ├── crdt/                 # vertical 02 — Automerge/yrs engine + reducers
│   ├── identity/             # vertical 07 — issuance/attenuation/revocation
│   ├── economy/              # vertical 05 — eUTXO + zk-WASM (OpenMesh only)
│   ├── tee/                  # opportunistic TDX/SEV/Secure-Enclave custody
│   └── cabi/                 # C-ABI surface exported to Go
│
├── components/               # WASM components (WIT + guest impls)
│   ├── wit/                  # *.wit world & interface definitions
│   └── examples/             # sample agent/worker components
│
├── tray/                     # vertical 10 — Tauri v2 tray app (Rust + minimal JS)
│
├── proto/                    # Protobuf (.proto) wire schemas
├── schemas/                  # CDDL for CBOR payloads, JSON-schema for config
│
├── docs/
│   ├── schemas/schemas.md    # consolidated schema reference
│   └── verticals/*.md        # 00..10 low-level designs
│
└── build/                    # cross-compile, packaging, signing
```

### 2.1 The two language boundaries

- **Go ↔ Rust (C-ABI / cgo):** Go calls into `core/cabi`. The surface is **narrow and capability-passing**: Go never receives raw pointers to secured resources, only opaque capability handles (`u64` table indices) it must present back. All memory ownership stays Rust-side.
- **Host ↔ Guest (WASM Component Model):** agent/worker code is compiled to **components** (not core modules). The host exposes capabilities as **typed `resource` handles** through WIT worlds. A guest can only touch what its world imports. This boundary *is* the sandbox.

---

## 3. Canonical Schemas

All schemas are defined once here (and consolidated in [docs/schemas/schemas.md](docs/schemas/schemas.md)). Internal high-throughput payloads use **Protobuf** (compact, fast) or **CBOR/CDDL** (for capability tokens needing canonical bytes for signing). Component interfaces use **WIT**.

### 3.1 Capability Token / Handle (CDDL — canonical CBOR, signed)

A capability is an **unforgeable, attenuable, revocable** reference. The wire form (for delegation across nodes) is a signed CBOR map; the in-process form is an opaque table index.

```cddl
capability = {
  ? v: uint .default 1,
  id:        bytes .size 16,         ; ULID/UUIDv7 of this capability
  resource:  resource-ref,           ; what it points at
  rights:    [+ right],              ; e.g. ["read"], ["alloc","exec"]
  caveats:   [* caveat],             ; attenuations (macaroon-style)
  parent:    bytes / null,           ; capability it was attenuated from
  issuer:    peer-id,                ; node that minted it
  nbf:       uint,                   ; not-before (unix sec)
  exp:       uint / null,            ; expiry
  nonce:     bytes .size 12,
  sig:       bytes .size 64          ; Ed25519 over the canonical encoding sans `sig`
}

resource-ref = { kind: rkind, node: peer-id, path: tstr, ? quota: quota }
rkind   = "vram" / "gpu" / "cpu" / "fs" / "audio" / "topic" / "wallet" / "killswitch"
quota   = { ? bytes: uint, ? flops: uint, ? secs: uint }  ; e.g. exactly 2 GiB VRAM
caveat  = { op: tstr, val: any }   ; e.g. {op:"max_bytes", val: 2147483648}
right   = "read" / "write" / "alloc" / "exec" / "mount" / "spend" / "revoke"
peer-id = bytes .size 32           ; Ed25519 public key
```

Attenuation rule: a derived capability's `rights ⊆ parent.rights` and its `caveats ⊇ parent.caveats` (strictly narrower). Verification walks the `parent` chain to a root-of-trust. See [07](docs/verticals/07-identity-cap-lifecycle.md).

### 3.2 Node Telemetry Packet (Proto3) — extends README §4.1

```proto
syntax = "proto3";
package cerberus.telemetry.v1;

message NodeTelemetry {
  bytes  peer_id      = 1;
  uint64 epoch_ms     = 2;
  Compute compute     = 3;
  Memory  memory      = 4;
  repeated Link links = 5;   // interconnect matrix
  Thermal thermal     = 6;
  Power   power        = 7;   // vertical 09
  VectorClock clock   = 8;   // causality for telemetry ordering
}
message Compute { uint32 p_cores=1; uint32 e_cores=2; uint32 npu_tops=3; double flops=4; double ipc=5; }
message Memory  { uint64 ram_total=1; uint64 ram_free=2; uint64 vram_total=3; uint64 vram_free=4; uint64 swap_free=5; }
message Link    { bytes peer=1; Medium medium=2; double mbps=3; double rtt_ms=4; uint32 mtu=5; }
enum   Medium   { WIFI=0; ETH=1; THUNDERBOLT=2; RDMA_TB=3; }
message Thermal { double cpu_c=1; double gpu_c=2; double headroom_c=3; bool throttling=4; }
message Power   { PowerSource src=1; double battery_pct=2; LidState lid=3; SleepHint hint=4; }
enum   PowerSource { AC=0; BATTERY=1; }
enum   LidState    { OPEN=0; CLOSED=1; }
enum   SleepHint   { AWAKE=0; IDLE=1; SLEEP_IMMINENT=2; }
message VectorClock { map<string,uint64> entries = 1; }
```

### 3.3 CRDT Op Envelope (Proto3)

```proto
message CrdtOp {
  bytes  doc_id    = 1;     // which memory document
  bytes  actor     = 2;     // peer-id of the writer
  VectorClock clock= 3;     // causal context
  string domain    = 4;     // selects the reducer (e.g. "agent.belief","kv","counter")
  bytes  delta     = 5;     // Automerge/yrs binary change
  bytes  cap       = 6;     // capability authorizing the write (topic resource)
  bytes  sig       = 7;     // Ed25519 over the op
}
```

Domain reducers (see [02](docs/verticals/02-distributed-state-crdts.md)) resolve merges. `domain="agent.belief"` triggers contradiction detection → human-flag event instead of silent LWW.

### 3.4 Compute Task Allocation + Promise (Proto3)

```proto
message ComputeTask {
  bytes  task_id      = 1;
  bytes  component    = 2;   // CID (IPLD) of the WASM component to run
  Shard  shard        = 3;   // tensor/pipeline shard descriptor
  repeated bytes caps = 4;   // capabilities granted to this task
  repeated Promise deps = 5; // promise-pipelined inputs (CapTP-style)
  bytes  result_cap   = 6;   // where to deliver / settle
}
message Shard   { ShardKind kind=1; uint32 layer_lo=2; uint32 layer_hi=3; uint32 tp_rank=4; uint32 tp_world=5; }
enum   ShardKind{ PIPELINE=0; TENSOR=1; DATA=2; }
message Promise { bytes promise_id=1; bytes producer=2; }  // resolved later, enables pipelining
```

### 3.5 9P Namespace Layout (control plane)

```
/cer
├── dev
│   ├── gpu/<peer>/<idx>/{ctl, info, alloc}     # alloc requires a "gpu"/"vram" capability
│   ├── vram/<peer>/<idx>/{ctl, info}           # ctl returns a data-plane endpoint, NOT bytes
│   └── audio/<peer>/{in, out, ctl}             # AES67/ROC stream descriptors
├── fs/...                                       # IPLD/Reed-Solomon distributed filesystem
├── proc/<task_id>/{ctl, status, caps}          # running components
└── cap/<cap_id>/{info}                          # introspect a held capability
```

`walk`/`open` on any path is **capability-checked** by the 9P server against the requester's held capabilities. Opening `dev/vram/.../ctl` returns a **data-plane endpoint** (QUIC stream id / RDMA handle), never tensor bytes. See [04](docs/verticals/04-9p-peripheral-virt.md).

### 3.6 eUTXO Settlement + zk-WASM Proof (CDDL — OpenMesh only)

```cddl
settlement-tx = {
  inputs:  [+ utxo-ref],            ; spent compute credits
  outputs: [+ utxo],                ; new credits / payment to provider
  proof:   zk-proof,                ; proves the compute was actually performed
  task:    bytes,                   ; task_id settled
  killswitch: bool .default false   ; on-chain revocation flag
}
utxo      = { owner: peer-id, value: uint, ? datum: any }
zk-proof  = { scheme: "zkwasm", program: bytes, public: [* any], proof: bytes }
```

`proof` binds the **component CID + inputs CID + outputs CID** so a provider cannot claim payment for compute it did not perform. zk-WASM proof-of-inference is **research-frontier** (≈100× overhead); production OpenMesh defaults to **optimistic settlement + fraud proofs**, with full zk-WASM as opt-in. See [05](docs/verticals/05-eutxo-open-mesh-economy.md).

### 3.7 Profile Config (JSON Schema)

```json
{
  "profile": "open_mesh | sealed",
  "economy":      { "enabled": true,  "chain": "open_mesh", "settlement": "optimistic|zkwasm" },
  "attestation":  { "enabled": false, "require_tee": false, "tee": ["tdx","sev","sep"] },
  "compliance":   { "audit_export": false, "retention_days": 0, "pii_redaction": false },
  "killswitch":   { "mode": "on_chain | governed" }
}
```
`profile=sealed` forces `economy.enabled=false` and `attestation.enabled=true` (validated at boot).

### 3.8 IPC Contract (daemon ↔ CLI ↔ tray)

Local-only transport: Unix domain socket (`$XDG_RUNTIME_DIR/cerberus.sock`) on Unix, named pipe on Windows; gRPC over it. **Every RPC carries a capability**; the CLI/tray hold a local *root operator* capability minted at install and stored in OS keychain / Secure Enclave. No TCP, no browser, no remote admin by default.

---

## 4. Final Integration — End-to-End Data-Flow Walkthroughs

These traces are normative: implementers MUST be able to reproduce them.

### 4.1 "Agent requests exactly 2 GiB of remote VRAM"

```
Agent(component)                Local cerberusd            Remote node B
   │                                  │                          │
   │ import vram.request(2GiB)        │                          │
   ├─────────────────────────────────►│ scheduler(06): pick B    │
   │                                  │ via telemetry(08)        │
   │                                  │ check caller holds a      │
   │                                  │ "vram" cap (00) ──────────►│ 9P walk /cer/dev/vram/B/0/ctl
   │                                  │                          │ cap-check (04)
   │                                  │◄───────── data-plane endpoint (QUIC/RDMA), attenuated
   │                                  │   cap: vram@B, quota{bytes:2GiB}  ◄─ NOT bytes
   │◄── resource handle (opaque) ─────┤                          │
   │ writes activations over data plane (QUIC zero-copy / RDMA-over-TB) ─────►│
   │ (OpenMesh) on completion → settlement-tx + proof (05) ──────────────────►│ chain
```
Key invariants: the agent never gets ambient access to B; it gets an attenuated capability scoped to exactly 2 GiB; tensor bytes never traverse 9P.

### 4.2 "A laptop drops its lid mid-pipeline"

```
lifecycle(09) on node C detects SleepHint=SLEEP_IMMINENT
   │
   ├─► CRDT(02): checkpoint C's agent-memory doc, broadcast final ops (vector clock sealed)
   ├─► supervisor(01): emit peer-down to Gossipsub; scheduler(06) marks C's shards lost
   ├─► scheduler(06): re-route C's pipeline layers to hot-standby D (capability re-minted to D)
   └─► on C wake: CRDT merge reconciles C's branch; contradictions → human-flag (never silent)
No central master is consulted; recovery is local + emergent.
```

### 4.3 "Open Mesh cross-org compute trade"

```
Org-X agent needs FLOPS → scheduler(06) finds Org-Y idle GPU on the inter-site libp2p mesh
   → Org-Y grants an attenuated "gpu" capability priced in compute credits
   → Org-X streams work (data plane), Org-Y returns results
   → settlement(05): eUTXO tx moves credits X→Y; zk/optimistic proof guards honesty
   → Protocol Tax skims the settlement (GTM)
```

---

## 5. Master Tech Stack

| Concern | Technology | Lang | Vertical |
|---|---|---|---|
| Daemon, CLI, supervision | Go 1.22+, custom OTP-style supervisor | Go | 01, 10 |
| Capability kernel / CapTP | hand-rolled OCap on Component Model | Rust | 00 |
| WASM execution | Wasmtime + Cranelift, component model, WASI P2 | Rust | 03 |
| AI acceleration | MLX (Apple), wgpu + tinygrad (PC/Linux) | Rust/native | 03 |
| Distributed memory | Automerge / `yrs` + vector clocks | Rust↔Go FFI | 02 |
| Intra-site fabric | Zenoh | Go | 01 |
| Inter-site fabric | go-libp2p (DCUtR, Gossipsub v1.1), quic-go | Go | 01 |
| Ephemeral leader election | hashicorp/raft (lease-scoped only) | Go | 01, 06 |
| Peripheral control plane | 9P2000.L, hanwen/go-fuse, WinFsp + cgofuse | Go | 04 |
| Distributed FS | JuiceFS-style split, klauspost/reedsolomon, IPLD | Go | 04 |
| Audio | PipeWire/CoreAudio capture, ROC/AES67 over QUIC | Go/native | 04 |
| Settlement (OpenMesh) | eUTXO chain, Delphinus-style zk-WASM | Rust | 05 |
| Content addressing | IPLD (dag-cbor), CIDs | Go | 04, 05 |
| TEE custody | Intel TDX / AMD SEV-SNP / Apple Secure Enclave | Rust | 00, 07 |
| Observability | OpenTelemetry-style spans over Zenoh | Go | 08 |
| Desktop UI | Tauri v2 (tray-first, native webview) | Rust + JS | 10 |
| Wire formats | Protobuf (throughput), CBOR/CDDL (signed caps), WIT (components) | — | all |

**Version posture:** pin minor versions; Wasmtime tracks component-model stabilization; libp2p/Zenoh tracked at latest stable; security crates audited (cargo-audit) in CI.

---

## 6. Why Cerberus Is Superior

| Dimension | Kubernetes / Ray / Spark | exo | Public cloud (AWS/GCP) | **Cerberus V2** |
|---|---|---|---|---|
| Topology | central master / head node | masterless (AI only) | central control plane | **masterless mesh** |
| Hardware | homogeneous assumed | heterogeneous (AI) | rented homogeneous | **heterogeneous, owned** |
| Security model | RBAC / identity / ambient | minimal | IAM / ambient | **object-capability, zero ambient authority** |
| Partition tolerance | poor (etcd quorum) | partial | N/A (centralized) | **CRDT-merged, partition-first** |
| Scope | general but stateless-leaning | LLM inference only | everything, metered | **agentic swarms: compute + memory + peripherals + economy** |
| Cost model | infra you run | free/local | idle-billed, egress-taxed | **free mesh + usage-proportional Protocol Tax** |
| Peripheral pooling | none | none | none | **9P device namespace (GPU/VRAM/audio/FS)** |
| Cross-org trustless compute | none | none | none | **eUTXO settlement + zk/optimistic proofs (OpenMesh)** |

The thesis: Kubernetes/Ray assume a benevolent homogeneous datacenter; the cloud assumes you will pay rent forever; exo solves only inference. **Cerberus is the only design that treats untrusted, heterogeneous, partition-prone, owned hardware as a first-class substrate for *continuous agent swarms* — secured by capabilities, not identities.**

---

## 7. Profiles (normative)

| Feature | `OpenMesh` | `Sealed` |
|---|---|---|
| eUTXO economy / wallets | **ON** | OFF (boot-rejected if enabled) |
| zk-WASM / optimistic settlement | ON | OFF |
| Cross-org compute trading | ON | OFF (intra-org only) |
| TEE attestation required | optional | **ON** |
| Audit export / retention | optional | **ON** |
| Kill-switch | on-chain capability | governed admin capability |
| Target | startups, dev mesh | infra grids, medical |

---

## 8. Maturity Matrix (honest margins)

| Capability | Status |
|---|---|
| Zenoh+libp2p hybrid fabric, OTP supervision | **Shippable** |
| OCap on WASM Component Model | **Shippable** (best-in-class) |
| CRDT memory merge | **Shippable** / semantic-conflict policy **Frontier** |
| WASM component execution, tensor/pipeline sharding | **Shippable** |
| Promise pipelining (CapTP) | **Shippable** |
| Real-time very-large-model inference over Wi-Fi | **Frontier** (latency-bound) |
| 9P control-plane namespace + FUSE/WinFsp | **Shippable** |
| RDMA-over-Thunderbolt data plane | **Shippable on Mac**, Frontier elsewhere |
| IPLD weight delivery | **Shippable** |
| eUTXO settlement (optimistic) | **Buildable** |
| zk-WASM proof-of-inference | **Frontier** (~100× overhead) |
| Host-level TEE memory shielding on consumer desktops | **Frontier** (hardware-limited) |
| Scheduling brain (baseline) | **Shippable** / multi-objective optimality **Frontier** |
| Distributed capability revocation | **Buildable** (propagation is the hard part) |

---

## 9. Vertical Index

| # | Doc | # | Doc |
|---|---|---|---|
| 00 | [OCap Security Kernel](docs/verticals/00-ocap-security-kernel.md) | 06 | [Placement & Scheduling](docs/verticals/06-placement-scheduling.md) |
| 01 | [Mesh Fabric & Transport](docs/verticals/01-mesh-fabric-transport.md) | 07 | [Identity & Capability Lifecycle](docs/verticals/07-identity-cap-lifecycle.md) |
| 02 | [Distributed State & CRDTs](docs/verticals/02-distributed-state-crdts.md) | 08 | [Observability & Tracing](docs/verticals/08-observability-tracing.md) |
| 03 | [Compute Orchestration](docs/verticals/03-compute-orchestration.md) | 09 | [Power, Thermal & Sleep](docs/verticals/09-power-thermal-sleep.md) |
| 04 | [9P Peripheral Virtualization](docs/verticals/04-9p-peripheral-virt.md) | 10 | [Desktop App Model](docs/verticals/10-desktop-app-model.md) |
| 05 | [eUTXO & Open Mesh Economy](docs/verticals/05-eutxo-open-mesh-economy.md) | — | [Consolidated schemas](docs/schemas/schemas.md) |
