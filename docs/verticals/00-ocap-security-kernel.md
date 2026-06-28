# Vertical 00 — OCap Security Kernel (The Spine)

> Owner archetype: **The Security Architect.** Conforms to [ARCHITECTURE.md](../../ARCHITECTURE.md). This is not a peripheral vertical — it is the **spine**: every other vertical holds, presents, attenuates, or revokes capabilities through this kernel. If you read one vertical doc, read this one.

## 1. Purpose & Responsibilities
- Provide an **object-capability (OCap)** security model: authority is conveyed *only* by possession of an unforgeable, attenuable reference. There are **no identities, roles, ACLs, or ambient authority** anywhere in Cerberus.
- Map OCap onto the **WASM Component Model** so that a capability is a typed `resource` handle a guest can hold but never forge or enumerate.
- Implement **CapTP-style** (Capability Transport Protocol) reference passing and **promise pipelining** across nodes.
- Mint, attenuate, verify, and enforce caveats on capabilities; integrate revocation ([07](07-identity-cap-lifecycle.md)).
- Opportunistically bind capabilities to **TEE-sealed** key custody.

## 2. Position in the System
- **Control plane.** Sits beneath every Go subsystem (via FFI) and every WASM guest (via WIT).
- Upstream: identity & lifecycle (07) supplies root-of-trust keys and revocation state.
- Downstream: 9P (04), compute (03), CRDT (02), economy (05), mesh topics (01) all gate on `verify()`.

## 3. Detailed Architecture
```
                ┌─────────────────────────────────────────────┐
                │              OCap Kernel (Rust)               │
                │                                               │
   guest WASM ──┤  CapTable  ── opaque u64 ⇄ Capability         │
   (component)  │     │                                         │
                │     ▼                                         │
                │  Verifier ── chain walk · sig · caveats       │
                │     │                                         │
   Go daemon ───┤  CapTP   ── export/import refs · promises     │──► remote node CapTP
   (FFI)        │     │                                         │
                │     ▼                                         │
                │  Sealer  ── TEE/keychain custody of root keys │
                └─────────────────────────────────────────────┘
```
- **CapTable:** per-component table mapping opaque indices → live capabilities. A guest names resources only by table index; indices are meaningless outside their component. This is the unforgeability mechanism at the language boundary (mirrors WASI's handle tables).
- **Verifier:** runs the algorithm in [schemas §1](../schemas/schemas.md). O(chain-depth); chains are short (typically ≤4).
- **CapTP:** the distributed object protocol. When node A grants node B a capability, CapTP exports a reference; B holds a proxy. Method calls on the proxy are messages; **promises** for not-yet-returned results can themselves be passed onward (pipelining; see [03](03-compute-orchestration.md)).
- **Sealer:** root signing keys never leave a TEE/keychain where available (Secure Enclave on Apple, TDX/SEV sealing on server-class).

## 4. Data Structures / Wire Formats
- `capability` (signed CBOR) — [schemas §1](../schemas/schemas.md). Ed25519 signatures; ULID ids; macaroon-style `caveats`.
- In-process handle: `u64` table index (opaque to guests).
- CapTP frames: `{op: deliver|deliver-only|resolve, target: export-id, args: [cap-ref|value], answer: promise-id}` over a Zenoh/libp2p stream, CBOR-encoded.

## 5. Interfaces / APIs
Rust (C-ABI exported to Go via `core/cabi`):
```rust
pub fn cap_mint(resource: ResourceRef, rights: Rights, caveats: &[Caveat]) -> CapId;
pub fn cap_attenuate(parent: CapId, drop: Rights, add: &[Caveat]) -> CapId;
pub fn cap_verify(cap: CapId, req: &Request, now: u64) -> Result<(), CapError>;
pub fn cap_revoke(cap: CapId) -> Result<(), CapError>;          // delegates to vertical 07
pub fn captp_export(cap: CapId, to: PeerId) -> ExportId;
pub fn captp_import(frame: &[u8]) -> Result<CapId, CapError>;
```
WIT (host→guest): the `caps` interface in [schemas §7](../schemas/schemas.md). Guests receive `resource` handles only.

## 6. Tech Stack
| Concern | Choice | Why |
|---|---|---|
| Language | Rust | memory safety for a security kernel; owns all secured memory |
| Runtime boundary | Wasmtime + Component Model | typed `resource` handles = capabilities; mature handle tables |
| Crypto | Ed25519 (`ed25519-dalek`), BLAKE3 | fast signing/hashing; canonical CBOR (`ciborium`) |
| Distributed objects | hand-rolled CapTP (OCapN-inspired) | no production Rust CapTP exists; model is well-specified |
| Key custody | Apple Secure Enclave / TPM / TDX-SEV sealing | keep root keys off addressable memory where possible |

## 7. Security Model
- **Threat model (achievable today):** buggy or compromised *agent* code. OCap + component sandbox confines it to exactly its granted handles. This is fully solved.
- **Threat model (frontier):** malicious *host*. Host-level memory shielding requires TEEs that are largely unavailable on consumer desktops (SGX deprecated; TDX/SEV server-only; Secure Enclave tiny). Cerberus uses TEEs **opportunistically for key custody**, and is honest that a fully malicious host on a consumer laptop can compromise resident agent memory. Sealed-profile deployments SHOULD run on TEE-capable hardware where this matters.
- **No ambient authority:** there is no global `open`, no node-wide admin token. The install-time operator capability ([10](10-desktop-app-model.md)) is itself attenuable and revocable.
- **Confused-deputy resistance:** because authority travels *with* the reference (not via identity lookups), the classic confused-deputy class is structurally absent.

## 8. Open Mesh vs Sealed
| | OpenMesh | Sealed |
|---|---|---|
| Root of trust | per-node self-sovereign key | org CA + TEE attestation required |
| `wallet`/`spend` capability | available | absent (economy off) |
| Kill-switch capability | on-chain | governed admin grant |
| Attestation on `captp_import` | optional | **required** (`ATTEST_FAILED` otherwise) |

## 9. Failure Modes & Mitigations
| Failure | Mitigation |
|---|---|
| Stolen capability token | short `exp`, revocation registry (07), TEE-sealed signing prevents minting forgeries |
| Revocation not yet propagated across partition | dispute window + optimistic deny on reconnect; caveats limit blast radius |
| CapTP proxy to a dead node | promises reject with `PARTITIONED`; supervisor (01) reaps |
| Caveat enforcement bug | caveats enforced at the kernel chokepoint, not per-caller; fuzzed in CI |

## 10. Verdict
- **OCap on the WASM Component Model: Shippable & best-in-class.** This is the strongest pillar in the entire system.
- **Full distributed CapTP + promise pipelining: Shippable but ambitious** — requires a hand-rolled protocol.
- **Host-TEE memory shielding on consumer desktops: Frontier** — hardware-limited; used opportunistically.

Open questions: canonical revocation-propagation latency target across partitions; whether to adopt OCapN wire format verbatim once it stabilizes.
