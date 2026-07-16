# Cerberus — Vision, Status & Roadmap

> **The one document for "where are we going, what's built, what's left, and who owns what."**
> Honest about maturity by design (per [ARCHITECTURE.md §8](ARCHITECTURE.md)). It pairs with:
> [ARCHITECTURE.md](ARCHITECTURE.md) (the canonical spec), [HANDOFF.md](HANDOFF.md) (deep technical "where things stand"),
> [CONTRACT.md](CONTRACT.md) (the frozen integration seam), and the per-feature designs in [docs/verticals/](docs/verticals/).

**Status legend** — used throughout:

| Tag | Meaning |
|---|---|
| ✅ **Real** | Built, wired into the daemon, and unit/E2E tested. |
| 🟡 **Partial** | A real core exists but key pieces are missing or not production-shaped. |
| 🧪 **Scaffold** | The structure/contract exists; the actual behaviour is a stub. |
| 🔭 **Frontier** | Research-grade. Documented stub by design; may stay partial (cost/hardware-limited). |

---

## 1. The Vision (the north star)

**Cerberus is a zero-trust, masterless, capability-secured distributed hypervisor that turns the heterogeneous machines you already own — Macs, PCs, Linux boxes, laptops — into a single private mesh that runs autonomous AI agent swarms, shares peripherals (GPU/VRAM/audio/storage), remembers across nodes, and (optionally) settles compute economically.**

It is the opposite of renting the cloud forever. You bind your own hardware into one secure "personal supercomputer," and software running on it can only ever touch what it has been explicitly *handed a key to* — never by identity or role, always by an unforgeable capability.

### What it lets people *do* (plain language)

These are the "products inside the product" — the end-user promises:

1. **"Make my machines act as one."** Launch the daemon on each box; they auto-discover on the network with zero configuration. No master, no cloud account.
2. **"Run this job on whichever machine is best."** Ship a sandboxed workload to the strongest/idlest node and get the result back — placement is automatic.
3. **"Run AI agent swarms on my own metal."** Long-lived agents that compute, remember, and coordinate across the mesh instead of in someone else's datacenter.
4. **"Pool my devices."** Use a remote machine's GPU/VRAM, microphone, speakers, or disk as if they were plugged into the machine in front of you.
5. **"Trust nothing by default."** Every cross-boundary action presents a capability you can mint, narrow ("read-only, 2 GB, expires in 1h"), delegate, and revoke. Survives reboots.
6. **"Stay running through chaos."** A laptop closing its lid or dropping Wi-Fi doesn't kill the swarm; state merges back on reconnect; contradictions are escalated to a human, never silently overwritten.
7. **"Optionally, trade compute."** In *Open Mesh* mode, idle capacity can be sold/bought across organisations with cryptographic settlement; in *Sealed* mode the economy is off and attestation is on (for regulated/medical/infra grids).

### The seven principles (non-negotiable)

No ambient authority · control plane ≠ data plane · masterless · partition-tolerance over consistency · heterogeneity is the default · zero configuration · two profiles (OpenMesh / Sealed) one codebase. (Full text: [ARCHITECTURE.md §1](ARCHITECTURE.md).)

---

## 2. The Vision in Full — the 11 capability pillars

Cerberus is specified as 11 interlocking "verticals." This is the complete vision, each with its dream and current maturity. (Designs: [docs/verticals/](docs/verticals/).)

| # | Pillar | The dream (what it ultimately enables) | Today |
|---|---|---|---|
| **00** | **OCap Security Kernel** | Every action in the system is gated by an unforgeable, attenuable, revocable Ed25519 capability — the "spine." No passwords, no roles, no ambient authority. | ✅ Real + now enforced into the Go daemon via cgo |
| **01** | **Mesh Fabric & Transport** | Zero-config discovery; Zenoh intra-site + libp2p inter-site; resilient masterless transport over chaotic Wi-Fi and across sites; OTP-style supervision. | 🟡 libp2p/QUIC discovery + supervision + **PeerID-bound sessions + cap-gated topics** real; Zenoh + raw-transport mTLS pending |
| **02** | **Distributed State & CRDTs** | Shared agent memory that merges across partitions with no master; contradictory beliefs flagged to humans, never silently resolved. | 🟡 LWW map + checkpoints + belief-conflict real; full Automerge/yrs merge pending |
| **03** | **Compute Orchestration** | Portable WASM components (not native binaries) run anywhere; tensor/pipeline sharding; promise pipelining; GPU abstraction via wgpu/MLX. | 🟡 Real WASM exec (wazero/wasmi) + **Wasmtime component model** + promise pipelining; WASI-P2 hook + GPU dispatch pending |
| **04** | **9P Peripheral Virtualization** | Remote GPU/VRAM/**audio (mic+speaker)**/storage as a capability-addressed file namespace mountable on Win/Mac/Linux; bytes flow over a separate fast data plane. | 🟡 Cap-gated namespace + **9P2000.L wire server** + **QUIC data plane** + **network audio** all real & tested; FUSE/WinFsp mounts + OS audio capture + distributed FS pending |
| **05** | **eUTXO & Open Mesh Economy** | Trustless cross-org compute trading; compute credits; optimistic/zk settlement so a provider can't bill for work it didn't do. | 🟡 Durable credit ledger real; advanced settlement (fraud/zk proofs) model-only |
| **06** | **Placement & Scheduling Brain** | Telemetry-driven, multi-objective placement; shard pipelines across nodes; reroute to hot-standbys on failure. | ✅ Cost-model placement + reroute + multi-shard pipeline real (baseline) |
| **07** | **Identity & Capability Lifecycle** | Issuance, attenuation, and *distributed revocation propagation*; key custody; attested admission of peers. | 🟡 Revocation OR-set + signed-challenge admission real (engine-side); Go-daemon bridge + real TEE attestation pending |
| **08** | **Observability & Tracing** | OpenTelemetry-style spans over the mesh; live topology, rings, and trace trees of swarm activity. | 🟡 Telemetry feed real; OTel export + trace UI pending |
| **09** | **Power, Thermal & Sleep** | Nodes react to lids closing, battery, and heat: checkpoint, hand back capabilities, promote standbys — the "lid-drop" recovery. | 🟡 Event-driven state machine + lid-drop→scheduler wiring real & tested; real OS power/thermal hooks stubbed |
| **10** | **Desktop App Model** | A polished tray-first desktop app (Tauri) + CLI + OpenAI-compatible gateway; install-and-go on every OS. | 🟡 Status API + dashboard code real; GUI not yet verified on real hardware; installers pending |
| — | **Profiles** | One binary, two modes: **OpenMesh** (economy on) and **Sealed** (economy off, attestation on). | ✅ Boot-time switch real (`-profile=`) |

---

## 3. What's Been Accomplished

The foundation — and notably the *hardest, most novel* part (capability security) — is real and tested. Built across five phases.

### Acceptance test (the v0.1 finish line) — ✅ passing
`go run ./test/e2e` boots **two real `cerberusd` processes**, they discover each other over the real mesh, and one runs `hello-shard.wasm` **remotely via real wazero**, returning `1337`. This is ARCHITECTURE §4 in miniature.

### Phase-by-phase

- **Frozen contract & schemas** ✅ — the cross-boundary types (capability CBOR/CDDL, telemetry/CRDT/compute proto3, WIT world, profile JSON-schema) are locked in [contract/](contract/), so all three workstreams build in parallel without colliding ([CONTRACT.md](CONTRACT.md)).
- **Real WASM core** ✅ — hand-rolled parser replaced with **wazero** (Go) and **wasmi** (Rust); the gateway runs real WASM.
- **Phase B — Multi-user auth** ✅ — `daemon/auth`: Ed25519 bearer **capability tokens** (mint/authorize/attenuate/revoke, expiry, scope, admin), enforced on the gateway (`:8080`), RPC (`:9092`), and status API (`:7777`). Operator token bootstrapped to the OS config dir; CLI authenticates.
- **Phase C — Durability** ✅ — `daemon/store` (bbolt): issuer key, revocations, the **eUTXO credit ledger**, and **CRDT checkpoints** all survive restart.
- **Phase D — Desktop dashboard** ✅ — `daemon/api` serves token-gated live status JSON; `tray/` is a real Tauri v2 app (Rust performs the authenticated fetch, the native webview renders).
- **Phase E — Real distributed substrate** ✅ (closed):
  - Multi-shard **pipeline placement** in the scheduler ✅.
  - **E1 — real Rust OCap kernel via cgo** ✅: the Go control plane's full `CapKernel` (mint/attenuate/verify/revoke) is backed by the Rust **Ed25519 `SignedKernel`** across a tiny C-ABI. `cerberusd -tags ffi` boots with `kernel=rust-signed-cabi`; capabilities are cryptographically enforced **end-to-end** ([docs/ffi.md](docs/ffi.md)).
  - **E2 cross-node compute over the mesh** ✅ (CID-addressed, HTTP exec path removed); **E3 PeerID-bound, capability-gated sessions** ✅; **E4 mesh revocation gossip** ✅ (revoke on node A → denied on node B); **lid-drop** wired ✅.
- **Phase F — Real compute & peripherals** ✅ (the MLP, GPU excepted):
  - **F1 Wasmtime** component-model executor ✅; **F3 capability + byte-quota-bound QUIC data plane** ✅; **F4 9P2000.L wire server** ✅; **F5 network audio** (jitter buffer + DLL drift control) ✅.
  - **Cross-cut wiring** ✅ — opening a 9P device `.../ctl` now mints a **real data-plane grant** (`dataplane.RegisterGrant`) and returns the dialable endpoint (was a placeholder); the 9P wire server **and** the QUIC data-plane receiver run under the supervisor; **audio rides the data plane** (`daemon/audiolink`, one session = one capability/quota-bound transfer). Control plane mints the grant, the data plane moves the bytes (ARCHITECTURE §4.1).
  - **F2 GPU dispatch** ⛔ deferred (needs real GPU hardware to validate honestly; not faked).
- **Composed daemon** ✅ — OCap + mesh (real libp2p/QUIC) + telemetry + scheduler + 9P + data plane run under an OTP-style supervisor (`daemon/system`).
- **Observability** ✅ — real **OpenTelemetry span export** (opt-in via `CERBERUS_TRACE`; replaces the no-op tracer) + a concurrent **load test** for the data-plane bridge.

### Green everywhere
`go build/vet/test ./...`, `cargo build/test --workspace`, `cargo fmt --check`, `cargo clippy -D warnings`, the 2-node E2E demo (→ `1337`), and `go test -race` on the concurrency-sensitive packages all pass.

---

## 4. What's Left (the gap to the dream)

Honest list of the missing/partial pieces, grouped by theme. Each maps to a roadmap phase in §5.

**A. Make the mesh genuinely distributed & secure**
- ✅ Real cross-node compute **runs over the mesh** — the E2E exec moved off local HTTP onto a capability-gated libp2p/QUIC stream (still returns `1337`).
- ✅ `ComputeTask.Component` **CID → wasm bytes** dispatched content-addressed over the mesh — *peer-to-peer IPLD fetch of missing blocks is still 🧪.*
- 🟡 **mTLS + PeerID** — PeerID-bound sessions are enforced on the libp2p/QUIC mesh; full custom-cert mTLS on the raw data-plane transport is still ⛔, and local RPC/gateway stay token-gated on localhost.
- 🧪 **Zenoh** intra-site fabric (only the libp2p path exists today).
- ✅ Cross-node **revocation propagation** (`sys/revocations` OR-set + mesh gossip) + signed-challenge admission — revoke on A denies on B. *(TEE-quote admission still 🔭.)*

**B. Make compute & peripherals real**
- ✅ **Wasmtime component model** (`core/runtime`, optional feature) — WASI-P2 still a documented hook (needs a `cargo-component` fixture).
- ⛔ **GPU dispatch** (wgpu / MLX) — the `gpu`/`vram` capability actually running work on a GPU. *The one MLP item left; needs real hardware.*
- ✅ **QUIC byte-quota data plane** (`daemon/dataplane`) — bulk transfer enforced before *and* during the stream; **wired to 9P `ctl` grants** and **carrying audio** (`daemon/audiolink`). *Remaining: PeerID-pin the data-plane TLS; true RDMA zero-copy is Frontier.*
- ✅ **9P wire server** (hugelgupf/p9) serving the cap-gated namespace under the supervisor — **mounts landed**: `cerberusd -mount` is a ✅ real FUSE mount on **Linux** (pure-Go go-fuse straight to `/dev/fuse`, no cgo; verified on WSL2), 🧪 **unverified on Windows** (code path exists via cgofuse→WinFsp; needs the driver), ⛔ not shipped on macOS (macFUSE needs a kext + reboot). Caveat that matters: **`/cer/fs` is not browsable through a mount** — fs files aren't enumerated and a read-open returns `ENOSYS` (`wire.go:256`); bytes ride the data plane by design.
- ✅ **Audio sharing (mic/speaker)** transport over the data plane (packetize + jitter-buffer + DLL clock-sync), **wired into a live daemon session**. OS capture/playback is now ✅ **real on Windows** (WASAPI) and ✅ **real on Linux** (PulseAudio native protocol, pure Go, no cgo). ⛔ **macOS CoreAudio is written but has NEVER been compiled or run** — it sits behind `//go:build darwin && cgo && cerberus_coreaudio` and a default macOS build gets the honest stub. PipeWire needs no separate backend: PipeWire ships `pipewire-pulse` (a PulseAudio-protocol server, on by default on every PipeWire distro), so one PulseAudio-protocol client covers both — see the reasoning in `daemon/audio/os_linux.go`.
- 🧪 **Distributed filesystem** (`/cer/fs`): IPLD + Reed-Solomon erasure coding.

**C. Make memory, economy & lifecycle production-shaped**
- 🟡 Full **CRDT memory** (Automerge/yrs) merge across real partitions + the belief-conflict human-flag UX.
- 🟡 **eUTXO settlement** live with optimistic **fraud proofs** (OpenMesh cross-org trade).
- 🟡 **Lid-drop** lifecycle wired (SLEEP_IMMINENT → checkpoint → scheduler standby promotion); the real **OS power/thermal/sleep** hooks (09) are still ⛔ a labelled stub.

**D. Make it a product (the unglamorous, essential layer)**
- ⛔ **Hardening:** key custody (TPM/Secure Enclave), rate limits/quotas on the data plane (byte-quota exists; per-principal rate limits don't), security audit + fuzzing, a written threat model.
- 🟡 **Ops:** real **OTel tracing exported** ✅ (opt-in `CERBERUS_TRACE`, concise log exporter — swap `WithBatcher` for OTLP); a data-plane bridge **load test** ✅; still ⛔ metrics/health endpoints, many-node load tests, and **chaos tests** (partition, lid-drop, node loss) as automated suites.
- ⛔ **Cross-platform reality:** actually build/run/verify on Mac + Windows + Linux; **launch & verify the Tauri GUI** on real hardware (never done headlessly); installers, code signing, auto-update.

**E. Frontier (research bets, opt-in, off the critical path)**
- 🔭 zk-WASM proof-of-inference (~100× overhead) · 🔭 host-TEE memory shielding (consumer-hardware-limited) · 🔭 RDMA-over-Thunderbolt (shippable on Mac, frontier elsewhere).

---

## 5. The Roadmap (phased, detailed)

Each phase has a **goal**, concrete **deliverables**, a **Definition of Done (DoD)** tied to a real demo, and its **dependencies**. Phases are ordered by dependency; within a phase, tracks (see §6) run in parallel.

### Phase E — Real Distributed Substrate *(in progress)*
**Goal:** two *separate machines* securely run a capability-gated job over the real mesh.
- **E1** ✅ Bind the real Rust OCap kernel via cgo (`-tags ffi`). *Done.*
- **E2** ✅ The e2e remote WASM exec now runs **over the mesh by CID** (`daemon/mesh/compute.go` + `daemon/wasm` content store), HTTP exec path removed, still returns 1337. Rust `core/runtime` CID store + promise pipelining also reachable via cgo. *Remaining hardening: cross-kernel signed-capability transfer on the wire; peer-to-peer IPLD fetch.*
- **E3** ✅ PeerID-bound sessions (libp2p/QUIC TLS, enforced) + capability-gated pub/sub topics, used by the composed daemon. *Remaining: full custom-cert mTLS over the future raw data-plane transport.*
- **E4** ✅ `sys/revocations` OR-set + **mesh gossip** (`daemon/auth.RevocationGossip`, wired in `cerberusd`): revoke on node A → cap-gated topic → denied on node B. Signed-challenge admission + cgo-reachable OR-set too.
- **E+** ✅ Lid-drop wired: `daemon/lifecycle` SLEEP_IMMINENT → checkpoint → `scheduler.RerouteNode` standby promotion (real OS power hooks still stubbed).

> **Phase E is essentially closed.** The remaining items above are hardening (signed cap transfer, Zenoh, OS power hooks), not blockers — the next focus is **Phase F (the MLP)**.
- **DoD:** reproduce ARCHITECTURE **§4.1 ("agent requests 2 GiB remote VRAM")** across two physical machines: cap-checked walk → attenuated endpoint → result returns; revoked cap is rejected mesh-wide.

### Phase F — Real Compute & Peripherals
**Goal:** actually use a remote GPU and a remote microphone.
- **F1** ✅ **Wasmtime** component-model executor (`core/runtime/wasmtime_exec`, optional `wasmtime` feature) alongside wasmi — runs a real component (fixture→1337). WASI-P2 = documented hook (needs a `cargo-component` fixture).
- **F2** ⛔ **Deferred** — **GPU dispatch** via wgpu/MLX needs a real GPU to validate honestly; not faked. Next pass on real hardware.
- **F3** ✅ **QUIC zero-copy data plane** (`daemon/dataplane`): capability + byte-quota-bound bulk transfer, enforced before *and* during the stream. The unlock for VRAM + audio sharing. *Remaining: PeerID-pin the data-plane TLS.*
- **F4** ✅ **9P2000.L wire server** (`daemon/ninep`, hugelgupf/p9) over the cap-gated namespace; per-connection capability; `ctl`→endpoint invariant held. *FUSE/WinFsp mount = labelled stub (kernel-driver-bound).*
- **F5** ✅ **Network audio** (`daemon/audio`): packetized sender/receiver, reordering jitter buffer, DLL drift control, gap-fill concealment. *OS capture: **real** on Windows (WASAPI) and Linux (PulseAudio protocol, pure Go — also covers PipeWire via pipewire-pulse). macOS CoreAudio is written but **never compiled**, behind an opt-in build tag.*
- **Cross-cut wiring** ✅ (`daemon/system`, `daemon/ninep`, `daemon/audiolink`): 9P `.../ctl` open → `dataplane.RegisterGrant` → returns the real dialable endpoint (ARCHITECTURE §4.1); **audio rides the data plane**; the 9P wire server **and** data-plane receiver are served under the daemon supervisor. End-to-end + race tested.
- **DoD (remaining):** open a remote GPU and run a real (small) inference shard (F2, real hardware); mount a peer device in Explorer/Finder (FUSE/WinFsp); stream a **live OS** mic across the mesh (the transport is done; OS capture is the stub left).

> ⭐ **Minimum Lovable Product (MLP) cut line — end of Phase F.**
> *"A small cluster of your own machines that securely runs sandboxed jobs and shares a GPU/mic across the LAN, visible in a desktop app."* This is the first genuinely demoable, lovable product — before full economy and full hardening.

### Phase G — Distributed Memory, Filesystem & Economy
**Goal:** persistent shared cognition + trustless trade + graceful failure.
- **G1** Full **CRDT memory** (Automerge/yrs) across real partitions; belief-conflict → human-flag UX in the tray.
- **G2** **Distributed FS** (`/cer/fs`): IPLD content-addressing + Reed-Solomon erasure coding scattered across peer free space.
- **G3** **Lifecycle (09)**: OS power/thermal/sleep events → CRDT checkpoint + capability hand-back + scheduler standby-promotion.
- **G4** **eUTXO settlement live** with **optimistic fraud proofs** (OpenMesh): cross-org compute trade settles in compute credits.
- **DoD:** reproduce ARCHITECTURE **§4.2 ("lid-drop mid-pipeline")** and **§4.3 ("cross-org compute trade")** end-to-end.

### Phase H — Hardening & Productionization
**Goal:** something a non-developer installs on three machines and trusts.
- **H1 Security:** issuer-key custody (TPM/Secure Enclave) + rotation; rate limits/quotas enforced on the data plane; external **security audit** + fuzzing; written threat model.
- **H2 Observability/Ops:** OTel traces exported; metrics/health endpoints; structured logging.
- **H3 Resilience suites:** automated **load tests** (many nodes, concurrent users) and **chaos tests** (partition, lid-drop, node loss) in CI.
- **H4 Cross-platform & packaging:** verified Mac/Win/Linux builds; **Tauri GUI verified on real hardware**; signed installers + auto-update; onboarding flow.
- **DoD:** a fresh user installs from a signed package on 3 machines and runs a workload from the tray, with traces, quotas, and recovery all working. → **v1.0**.

### Phase I — Frontier (opt-in, parallelizable, never blocking)
- **I1** zk-WASM proof-of-inference (opt-in settlement mode). **I2** Host-TEE memory shielding (where hardware allows). **I3** RDMA-over-Thunderbolt data plane (Mac first).
- **DoD:** each is documented, benchmarked, opt-in, and off the critical path.

---

## 6. Team & Ownership (three tracks)

The repo is already structured into three workstreams that build standalone against the frozen contract ([docs/workstreams.md](docs/workstreams.md)). We map the team onto them. Each track owns a coherent club of verticals end-to-end — design, code, tests, and its slice of every roadmap phase.

### 🟦 Track A — Core, Security & Integration **(You)**
**Charter:** the substrate that thinks, computes, and grants — plus owning the contract and integration. This is the spine the other two tracks plug into.
- **Owns verticals:** 00 OCap kernel · 02 CRDT engine · 03 Compute orchestration · 04 9P namespace logic · 06 Scheduling brain.
- **Owns in the contract:** capability schema + `cap_verify/mint/attenuate`; the WIT `caps` world; CRDT op envelope; compute task + promise; 9P op→cap table.
- **Cross-cutting:** the Go↔Rust cgo keystone (done), the integration ladder, release management, and the architecture as a whole.
- **Roadmap slice:** E1 ✅, E2 (compute on mesh + IPLD + promises), F1 (Wasmtime), F2 (GPU dispatch), G1 (CRDT memory), and final integration sign-off each phase.

### 🟩 Track B — Fabric, Transport & Trust **(Member 2)**
**Charter:** how nodes find each other, prove who they are, move bytes fast, and are observed. The wire and the lens — everything depends on it.
- **Owns verticals:** 01 Mesh fabric & transport · 07 Identity & capability lifecycle · 08 Observability & tracing.
- **Owns in the contract:** Zenoh key space + topic-cap gating; telemetry publication cadence; attestation envelope; `sys/revocations` OR-set; OTel span format + trace-context propagation.
- **Roadmap slice:** E3 (mTLS + PeerID), E4 (revocation propagation + attested admission), F3 (QUIC zero-copy data plane — the device-sharing unlock), H2 (OTel export), H3 (chaos/partition suites). Zenoh intra-site fabric is theirs to bring online.

### 🟪 Track C — Economy, Lifecycle & Experience **(Member 3)**
**Charter:** the optional economy, the node's physical-edge behaviour, and the entire human/external surface — the parts a user actually sees and the deployment can mix-and-match.
- **Owns verticals:** 05 eUTXO & Open Mesh economy · 09 Power/thermal/sleep lifecycle · 10 Desktop app (daemon/CLI/tray/gateway).
- **Owns in the contract:** settlement tx + zk/optimistic proof envelope; wallet WIT resource; lifecycle events; IPC gRPC + operator-capability bootstrap; OpenAI-compatible gateway shape.
- **Roadmap slice:** F4/F5 surface (device mounts + audio UX), G3 (lifecycle "lid-drop"), G4 (live eUTXO settlement + fraud proofs), H4 (cross-platform packaging, signed installers, **Tauri GUI on real hardware**, onboarding). The peripheral-pooling and audio demos are theirs to make *feel* like a product.

> **Seams (so tracks never block each other):** B & C consume A's capability kernel — until wired, they use the stub cap kernel that honours the contract. A consumes B's transport/telemetry/identity — until wired, in-process loopback + synthetic feeds + an always-valid verifier. This is the whole point of the frozen contract: three people, parallel, no collisions.

---

## 7. Sequencing — who does what, when

| Phase | Track A (Core/Sec/Integ) | Track B (Fabric/Trust) | Track C (Economy/Surface) |
|---|---|---|---|
| **E** *(now)* | E1 ✅ cgo kernel; E2 compute-on-mesh + IPLD + promises | E3 mTLS/PeerID; E4 revocation OR-set + admission | Wire CLI/gateway/tray to the real kernel; sealed/openmesh boot validation |
| **F** *(MLP)* | F1 Wasmtime + WASI P2; F2 GPU dispatch | F3 QUIC zero-copy data plane; Zenoh intra-site | F4 device mounts (FUSE/WinFsp) UX; F5 audio mic/speaker demo |
| **G** | G1 CRDT memory merge + conflict flag | FS replication signalling; trace tree for memory | G2 distributed FS UX; G3 lid-drop lifecycle; G4 eUTXO settlement |
| **H** | Threat model + key custody review; integration sign-off | H2 OTel export; H3 chaos/load suites | H1 quotas/rate-limits surface; H4 installers + GUI verification |
| **I** | zk-WASM hooks in settlement contract | RDMA-over-TB transport (Mac) | TEE attestation surfacing; opt-in proof UX |

---

## 8. Risks & honest unknowns

- **Frontier features may never be "cheap."** zk-WASM (~100× overhead) and host-TEE on consumer hardware are research bets — kept opt-in and off the critical path on purpose. Don't stake the product on them.
- **Real-time large-model inference over Wi-Fi is latency-bound** (ARCHITECTURE §8). The realistic near-term compute story is *sharded/batched* work and *smaller* models, not live giant-model serving over flaky links.
- **The data plane is the long pole.** QUIC zero-copy (F3) unblocks VRAM *and* audio *and* FS at once — prioritise it; many "wow" demos are gated behind it.
- **Everything so far is validated on one box.** Multi-machine, multi-OS reality (Phase H) routinely surfaces issues simulations hide. Budget for it.
- **The GUI is unproven on real hardware.** The Tauri app is real code but has not been launched/verified in a desktop session here — treat "it renders and feels good" as unverified until Phase H4.

---

## 9. Appendix

### Repo map
```
cmd/        cerberusd (daemon), cerberus (CLI)
daemon/     auth ffi gateway api store ledger state system scheduler ninep mesh dataplane audio audiolink telemetry supervisor lifecycle economy   (Go)
core/       ocap crdt runtime identity economy cabi                                                                      (Rust)
contract/   go + rust — FROZEN integration types (see CONTRACT.md)
proto/ components/wit/ schemas/   frozen contract sources
tray/       Tauri v2 desktop app
test/e2e    2-process acceptance demo
docs/       ARCHITECTURE refs, verticals/ (00–10), workstreams.md, ffi.md
```

### Commands
```
go build ./...              # build all Go
go test ./...               # all Go unit tests
cargo build --workspace     # Rust core crates
cargo test --workspace
go run ./test/e2e           # 2-node remote-WASM acceptance demo (prints 1337)
pwsh build/ffi.ps1 -Action test    # real Rust OCap kernel over cgo (see docs/ffi.md)
# desktop UI (needs Tauri toolchain + WebView2): run cmd/cerberusd, then  cd tray && cargo tauri dev
```

### Glossary
- **Capability (OCap):** an unforgeable, attenuable, revocable key to a specific resource — the only way to act. No identities, no roles.
- **Attenuation:** narrowing a capability (drop rights / add caveats) before delegating — a child can never exceed its parent.
- **Control plane vs data plane:** the control plane *names, grants, schedules* (small messages, 9P/Zenoh); the data plane *moves bytes* (QUIC/RDMA). Never conflated.
- **CRDT:** conflict-free replicated data type — lets nodes edit shared memory offline and merge deterministically on reconnect.
- **eUTXO:** the extended-UTXO accounting model behind compute credits and settlement (OpenMesh only).
- **Profiles:** `OpenMesh` (economy on) vs `Sealed` (economy off, attestation on) — one binary, boot-time switch.
- **MLP:** Minimum Lovable Product — the end-of-Phase-F cut where this becomes a thing people *want* to run, not just a demo.
