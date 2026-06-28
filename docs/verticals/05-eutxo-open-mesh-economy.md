# Vertical 05 — eUTXO Settlement & the Open Mesh Economy (Optional)

> Owner archetype: **The Cryptoeconomist.** Conforms to [ARCHITECTURE.md](../../ARCHITECTURE.md). **This vertical is profile-gated: ON in `OpenMesh`, entirely OFF in `Sealed`.** It is never load-bearing for the regulated verticals.

## 1. Purpose & Responsibilities
- Enable **trustless off-chain compute settlement** between mutually-distrusting nodes/orgs: a provider runs work, a consumer pays compute credits, and neither must trust the other.
- Embed **per-agent cross-chain wallets**; settle into **eUTXO** smart contracts.
- Police honesty via **anti-cheat proofs** (optimistic fraud proofs by default; **zk-WASM** validity proofs as opt-in frontier).
- Deliver model weights peer-to-peer via **IPLD** (BitTorrent-for-weights), independent of profile.
- Provide an **on-chain kill-switch** capability.

## 2. Position in the System
- **Economy layer**, above orchestration. Settles tasks completed by compute (03) using endpoints granted via 9P (04).
- Upstream: OCap (00) issues `wallet`/`spend` capabilities. Downstream: GTM Protocol Tax skims settlements.

## 3. Detailed Architecture
```
   Consumer agent (wallet cap)        Provider node (idle GPU)
        │  request priced compute            │
        ├───────────────────────────────────►│  scheduler(06)+9P(04) grant gpu cap
        │  stream work (data plane, 03)       │  run component (CID-addressed)
        │◄──────── results + claim ───────────┤
        │                                     │
        ▼  SETTLEMENT                          │
   ┌──────────────── eUTXO contract ──────────────┐
   │ inputs: consumer credit UTXOs                  │
   │ outputs: provider payment UTXO + change        │
   │ proof:  optimistic  (default)                  │
   │         | zk-WASM    (opt-in, ~100× overhead)  │
   │ killswitch: bool                               │
   └────────────────────────────────────────────────┘
        ▲ Protocol Tax skims a small fee (GTM)
```
**Why eUTXO (not account-model):** the **extended UTXO** model makes each settlement a self-contained, deterministically-validatable transaction — no global mutable account state, parallelizable, and the validity of a spend is a pure function of its inputs + datum + proof. This matches a partition-prone mesh far better than an account/nonce model.

**Settlement modes:**
- **Optimistic (default, Buildable):** provider is paid on claim; a **dispute window** lets any challenger submit a **fraud proof** (re-execute the CID-addressed component on the CID-addressed inputs and show output mismatch) to slash the provider's bond. Cheap, practical.
- **zk-WASM (opt-in, Frontier):** provider attaches a zero-knowledge validity proof that the component executed correctly on the inputs, binding `component CID · input CID · output CID`. Mathematically airtight but ~100× compute overhead — reserved for high-value or adversarial settlements.

**IPLD weight delivery:** large model weights are chunked, content-addressed (CID), and pulled from the nearest peers holding the blocks (Reed-Solomon redundant, [04](04-9p-peripheral-virt.md)) instead of from a central server. Works in both profiles.

## 4. Data Structures / Wire Formats
- `settlement-tx`, `utxo`, `zk-proof` — [schemas §6](../schemas/schemas.md).
- Wallet capability: `resource wallet { balance, spend }` ([schemas §7](../schemas/schemas.md)).
- Kill-switch: a `killswitch` capability whose exercise writes an on-chain revocation (OpenMesh) consumed by identity (07).

## 5. Interfaces / APIs
WIT (guest, OpenMesh only): `wallet.spend(to, amt) -> proof`.
Rust core:
```rust
pub fn settle(task: TaskId, claim: Claim, mode: SettleMode) -> Result<TxId, EconError>;
pub fn challenge(tx: TxId, fraud: FraudProof) -> Result<Slash, EconError>;  // optimistic
pub fn weights_fetch(cid: Cid) -> Result<ByteStream, EconError>;           // IPLD, any profile
```

## 6. Tech Stack
| Concern | Choice | Why |
|---|---|---|
| Settlement model | **eUTXO** smart contracts | deterministic, parallel, partition-friendly |
| Anti-cheat (default) | optimistic + fraud proofs (re-exec CID) | cheap, practical anti-cheat |
| Anti-cheat (opt-in) | **zk-WASM** (Delphinus-style) | trustless validity proof; frontier overhead |
| Weight delivery | **IPLD** (dag-cbor, CIDs) + Reed-Solomon | decentralized, dedup, peer-sourced |
| Wallets | cross-chain key custody (TEE-sealed where possible, [00](00-ocap-security-kernel.md)) | per-agent autonomous spend |

## 7. Security Model
- `spend` is a capability with quota caveats (max amount, rate); the kernel enforces it — a runaway agent cannot drain a wallet.
- Fraud proofs make cheating provably unprofitable (bond slash > expected gain).
- Kill-switch is itself a capability — exercising it requires holding it; in OpenMesh it is on-chain and auditable.
- Replay/double-spend prevented by the eUTXO model (an input is consumed exactly once).

## 8. Open Mesh vs Sealed
| | OpenMesh | Sealed |
|---|---|---|
| Settlement / wallets | **ON** | **OFF** (boot-rejected if enabled) |
| zk-WASM / fraud proofs | available | n/a |
| Kill-switch | on-chain capability | governed admin capability ([07](07-identity-cap-lifecycle.md)) |
| IPLD weight delivery | ON | **ON** (profile-independent) |

## 9. Failure Modes & Mitigations
| Failure | Mitigation |
|---|---|
| Provider fakes compute | optimistic fraud proof (re-exec) or zk-WASM validity proof |
| Consumer refuses to pay | escrowed UTXO inputs locked before work starts |
| zk-WASM too slow | default to optimistic; zk reserved for high-value/adversarial |
| Chain unreachable in partition | settlement queued; eUTXO txs are self-validating and submitted on reconnect |
| Wallet key theft | TEE-sealed custody; capability quota caps loss |

## 10. Verdict
- **IPLD weight delivery: Shippable.**
- **eUTXO + optimistic settlement: Buildable.**
- **zk-WASM proof-of-inference: Frontier** (~100× overhead) — included honestly, not as a free lunch.
- **Whole vertical: optional and profile-gated** — the regulated verticals never touch it.

Open questions: which eUTXO chain (Cardano-family vs purpose-built L2) for the credit ledger; dispute-window length vs settlement latency.
