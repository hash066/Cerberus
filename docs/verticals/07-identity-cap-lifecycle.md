# Vertical 07 — Identity & Capability Lifecycle (NEW)

> **Net-new vertical.** OCap ([00](00-ocap-security-kernel.md)) is only as strong as its issuance and revocation story. This vertical owns the *lifecycle*: bootstrap, mint, attenuate, delegate, rotate, revoke. Conforms to [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Purpose & Responsibilities
- Establish each node's **root of trust** (self-sovereign key in OpenMesh; org-CA + TEE attestation in Sealed).
- Manage the full **capability lifecycle**: issuance, attenuation, delegation, expiry, **revocation**, and key rotation.
- Solve the **bootstrap problem**: who mints the very first capability, and how a fresh node is admitted.
- Propagate **revocation** across a partition-prone mesh.

## 2. Position in the System
- **Control plane**, paired tightly with the OCap kernel (00).
- Upstream: TEE custody. Downstream: every vertical that calls `cap_verify` consults the revocation registry here.

## 3. Detailed Architecture
```
   Node boot
     │
     ├─ generate / unseal node keypair (Secure Enclave/TPM/TDX where present)
     │
     ├─ OpenMesh: PeerID = self key (self-sovereign root)
     │  Sealed:   request org-CA cert + TEE attestation quote → admitted iff valid
     │
     ▼
   Root capability  ──attenuate──►  service caps  ──delegate(CapTP)──►  agent/worker caps
     │                                                                       │
     └────────────────────────── Revocation Registry ◄──────────────────────┘
                  (CRDT OR-set of revoked cap-ids + reasons; gossiped via 01)
```
- **Bootstrap:** the install-time **operator capability** ([10](10-desktop-app-model.md)) is the local root; it can mint service capabilities. A new node joining a Sealed org must present a **TEE attestation quote** verified against the org CA before any capability is delegated to it (`ATTEST_FAILED` otherwise).
- **Attenuation/delegation:** handled by the kernel (00); this vertical records provenance (parent chains) and enforces the monotone-narrowing invariant at issuance time.
- **Revocation:** modeled as a **CRDT OR-set** of revoked capability ids (so it converges across partitions, [02](02-distributed-state-crdts.md)) plus short capability `exp` times so the *window* of a stale revocation is bounded. On reconnect, the union of revocation sets applies; verifiers fail-closed on any revoked id in the chain.
- **Rotation:** node keys rotate on schedule or on suspected compromise; old keys enter a grace overlap; capabilities signed by a rotated-out key are re-issued or expire.

## 4. Data Structures / Wire Formats
- `capability` + `parent` chain — [schemas §1](../schemas/schemas.md).
- Revocation entry: `{cap_id, reason, revoked_by, clock}` in an OR-set CRDT doc `sys/revocations`.
- Attestation quote: platform-specific (TDX TD-REPORT / SEV attestation / Secure Enclave assertion), wrapped in `{platform, quote, nonce, cert_chain}`.

## 5. Interfaces / APIs (Rust core, exposed via FFI)
```rust
pub fn bootstrap(profile: Profile) -> Result<RootCap, IdError>;
pub fn admit_peer(quote: &Attestation, ca: &OrgCa) -> Result<PeerId, IdError>;  // Sealed
pub fn revoke(cap: CapId, reason: Reason) -> Result<(), IdError>;
pub fn is_revoked(cap: CapId) -> bool;                 // consulted by cap_verify (00)
pub fn rotate_node_key() -> Result<KeyId, IdError>;
```

## 6. Tech Stack
| Concern | Choice | Why |
|---|---|---|
| Language | Rust | shares the security core with [00](00-ocap-security-kernel.md) |
| Keys | Ed25519, X25519 | signing + key agreement |
| Custody | Secure Enclave / TPM 2.0 / TDX-SEV sealing | keep roots off addressable memory where possible |
| Revocation store | CRDT OR-set ([02](02-distributed-state-crdts.md)) | partition-tolerant convergence |
| Attestation | Intel TDX / AMD SEV-SNP / Apple SEP | Sealed-profile admission |

## 7. Security Model
- **Fail-closed:** any capability whose chain contains a revoked id, or whose signature/attestation fails, is denied.
- **Bounded staleness:** short `exp` + gossiped OR-set means a revoked capability cannot be used indefinitely even across a partition.
- **No super-admin:** even the operator capability is attenuable and revocable; there is no unrevocable god key.

## 8. Open Mesh vs Sealed
| | OpenMesh | Sealed |
|---|---|---|
| Root of trust | self-sovereign node key | org CA + **TEE attestation required** |
| Peer admission | open (capability-gated) | attested-only |
| Revocation | gossiped OR-set | OR-set + audit log + retention |
| Kill-switch | on-chain ([05](05-eutxo-open-mesh-economy.md)) | governed admin revocation |

## 9. Failure Modes & Mitigations
| Failure | Mitigation |
|---|---|
| Revocation slow to propagate across partition | short `exp` bounds window; fail-closed on reconnect union |
| Lost/destroyed root key | M-of-N social/operator recovery; re-attestation in Sealed |
| Attestation forgery (Sealed) | verify quote against vendor + org CA; nonce freshness |
| Key compromise | rotation with grace overlap; mass-revoke by key id |

## 10. Verdict
- **Issuance / attenuation / delegation / rotation: Shippable.**
- **Distributed revocation propagation: Buildable** — the genuinely hard part; mitigated by short expiries + CRDT convergence, but instantaneous global revocation across a partition is impossible (CAP), so the design bounds the window rather than pretending to eliminate it.

Open questions: default `exp` tuning per resource class; whether to add threshold (M-of-N) issuance for high-value capabilities.
