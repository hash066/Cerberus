# Cerberus — Launch Plan ("Road to v1.0")

> The engineering lead's execution plan for the final push. Pairs with
> [VISION-AND-ROADMAP.md](VISION-AND-ROADMAP.md) (the full roadmap) and
> [HANDOFF.md](HANDOFF.md) (current truth). Phases A–F + the data-plane cross-cut
> + OTel export are **done and on `integration`**. This plan closes Phase G + H
> and the remaining hardening to a shippable v1.0.

## Operating model
Work is fanned out to parallel **squads**, each owning **disjoint directories**,
building and testing **standalone** against the frozen contract. The lead
integrates at the seams, keeps the whole tree green (`go build/vet/test ./...`,
`cargo build/test/fmt/clippy --workspace`, `go run ./test/e2e`, `build/ffi.ps1`),
and pushes. Hardware/OS edges that can't be validated headlessly (real GPU,
FUSE/WinFsp mount, OS audio capture, Tauri GUI on hardware, TEE) are **honestly
stubbed with labelled extension points, never faked**.

## Squads (this wave — parallel)

| Squad | Charter | Owns (dirs) |
|---|---|---|
| **G1 Memory** | Full CRDT memory merge across partitions + belief-conflict surfacing | `core/crdt`, `daemon/state` |
| **G2 Storage** | Distributed FS `/cer/fs`: IPLD chunking + Reed-Solomon erasure coding | `daemon/dfs` (new) |
| **G3 Economy** | eUTXO live settlement + optimistic fraud proofs (cross-org trade) | `core/economy`, `daemon/ledger`, `daemon/economy` |
| **Sec** | Signed capability envelope on the wire (replace shared-kernel demo model) + key-custody scaffold | `daemon/auth` |
| **Obs** | Metrics + health endpoints + trace tree (OTel export already landed) | `daemon/telemetry`, `daemon/metrics` (new) |
| **Chaos** | Multi-node simulation: chaos (partition / node-loss / lid-drop) + load suites | `test/chaos` (new), `test/load` (new) |
| **Release** | CI pipeline (build/test/lint/e2e/ffi) + packaging/installer scaffolding | `.github/`, `build/` |
| **Runtime** | WASI-P2 component (cargo-component fixture) + GPU-dispatch abstraction (software fallback; real wgpu behind a feature) | `core/runtime` |

The lead reserves `cmd/`, `daemon/system`, `daemon/mesh`, `daemon/dataplane`,
`daemon/ninep`, `daemon/gateway`, and the frozen contract for **integration
cross-wiring** (signed caps into mesh/dataplane; `/cer/fs` into the 9P namespace;
settlement into compute completion; metrics into the daemon).

## Launch criteria (v1.0 Definition of Done)
1. **Green everywhere** incl. the ffi build and an automated **CI** run.
2. **The three ARCHITECTURE §4 walkthroughs reproduce** in a multi-node
   simulation: §4.1 remote-resource grant over the data plane, §4.2 lid-drop
   re-route, §4.3 cross-org settled trade.
3. **Security:** caps are signed on the wire; data-plane mTLS PeerID-pinned;
   rate-limits/quotas enforced; a written threat model; `cargo audit` clean.
4. **Resilience:** chaos + load suites pass in CI.
5. **Packaging:** signed installers build for Mac/Win/Linux; the Tauri GUI is
   verified on real hardware (the one gate that needs a human + a desktop).
6. **Honesty ledger:** every remaining stub (GPU, mounts, OS audio, TEE,
   zk-WASM, RDMA) is explicitly listed as Frontier/next, not hidden.

## After this wave (sequenced)
- **Integration pass:** cross-wire the squads' outputs into the daemon; full
  green + e2e + ffi; push.
- **Phase F2 (GPU)** on a real GPU box; **FUSE/WinFsp** mounts; **OS audio**
  capture — the hardware-bound trio, on real machines.
- **Real multi-machine bring-up** (3 boxes, mixed OS) + GUI verification.
- **Phase I frontier** (zk-WASM, host-TEE, RDMA-over-TB) as opt-in research.
