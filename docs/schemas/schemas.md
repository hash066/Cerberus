# Cerberus — Consolidated Schema Reference

> 🔒 **FROZEN INTEGRATION CONTRACT.** This file (with [ARCHITECTURE.md §3](../../ARCHITECTURE.md)) is the contract every implementation workstream codes against — see [docs/workstreams.md](../workstreams.md). Changes are a cross-team event: propose → review by all workstreams → version-bump → adopt. Do not edit locally to suit one workstream.

> Single source of truth for every wire format, file format, and interface contract. Normative definitions also appear in [ARCHITECTURE.md §3](../../ARCHITECTURE.md); this file is the implementer's quick-reference and adds the WIT worlds, error codes, and verification pseudocode that the per-vertical docs cite.

Encoding policy:
- **Protobuf (proto3)** — high-throughput internal payloads (telemetry, CRDT ops, tasks).
- **CBOR + CDDL** — anything that must be *canonically* serialized for signing (capabilities, settlement).
- **WIT** — host↔guest component interfaces (the sandbox boundary).
- **JSON Schema** — human-edited config.

---

## 1. Capability (CBOR / CDDL)

See [ARCHITECTURE.md §3.1](../../ARCHITECTURE.md) for the full `capability` CDDL. Verification algorithm (host-side, [vertical 00](../verticals/00-ocap-security-kernel.md)):

```
fn verify(cap, now, registry):
    assert cap.nbf <= now and (cap.exp is null or now < cap.exp)
    assert not registry.revoked(cap.id)              # vertical 07
    chain = []
    cur = cap
    while cur.parent is not null:
        parent = registry.get(cur.parent)
        assert subset(cur.rights, parent.rights)     # monotone attenuation
        assert superset(cur.caveats, parent.caveats) # strictly narrower
        assert verify_sig(cur)                        # Ed25519
        chain.push(cur); cur = parent
    assert registry.is_root_of_trust(cur.issuer)     # terminates at a node root key
    assert verify_sig(cur)
    return enforce_caveats(cap, request)             # e.g. max_bytes, max_flops
```

Right lattice: `read ⊂ {read,write} ⊂ {read,write,alloc}`; `exec`, `mount`, `spend`, `revoke` are orthogonal grants. A child may only *drop* rights and *add* caveats.

---

## 2. Telemetry (proto3)

Full message in [ARCHITECTURE.md §3.2](../../ARCHITECTURE.md). Published on Zenoh key `cerberus/telemetry/<peer>` at 1–4 Hz (back-pressure-aware; [vertical 08](../verticals/08-observability-tracing.md)). Consumers: scheduler (06), lifecycle (09), tray (10).

---

## 3. CRDT Op Envelope (proto3)

Full message in [ARCHITECTURE.md §3.3](../../ARCHITECTURE.md). `domain` selects a reducer:

| `domain` | Reducer | Conflict policy |
|---|---|---|
| `kv` | last-writer-wins register | LWW by (clock, actor) |
| `counter` | PN-counter | additive, conflict-free |
| `set` | OR-set | add-wins |
| `agent.belief` | domain reducer | **contradiction → human-flag event**, no silent merge |
| `log` | append-only RGA | causal order |

---

## 4. Compute Task & Promise (proto3)

Full message in [ARCHITECTURE.md §3.4](../../ARCHITECTURE.md). A `Promise` is an unresolved future referenced before its producer finishes — enabling **promise pipelining** ([vertical 03](../verticals/03-compute-orchestration.md)): downstream shards are dispatched with promise handles, so a Wi-Fi round-trip is not paid per hop.

---

## 5. 9P Namespace (control plane)

Full tree in [ARCHITECTURE.md §3.5](../../ARCHITECTURE.md). Operation semantics ([vertical 04](../verticals/04-9p-peripheral-virt.md)):

| 9P op | Effect | Capability required |
|---|---|---|
| `walk` to `/cer/dev/vram/B/0` | enumerate device | `read` on `vram@B` |
| `open .../ctl` | obtain **data-plane endpoint** (QUIC/RDMA) | `alloc` on `vram@B` with `quota.bytes` |
| `read .../info` | static device descriptor | `read` |
| `open /cer/fs/<path>` | open distributed file | `read`/`write` on `fs` |

Invariant: **no tensor/VRAM bytes traverse 9P** — `ctl` yields an endpoint; the data plane carries bytes.

---

## 6. Settlement & zk-WASM (CBOR / CDDL — OpenMesh)

Full `settlement-tx` in [ARCHITECTURE.md §3.6](../../ARCHITECTURE.md). Two settlement modes ([vertical 05](../verticals/05-eutxo-open-mesh-economy.md)):

- `optimistic` — provider is paid on claim; a challenger may submit a fraud proof within a dispute window to slash. **Default.**
- `zkwasm` — provider must attach a validity proof binding `component CID · input CID · output CID`. **Frontier** (~100× overhead).

---

## 7. WIT Worlds (host ↔ guest sandbox boundary)

The capability spine expressed as component-model types. A guest sees only what its world imports — that *is* the sandbox.

```wit
package cerberus:agent@0.2.0;

interface caps {
  // capabilities are opaque, unforgeable resource handles
  resource vram   { info: func() -> quota; }
  resource gpu    { submit: func(work: list<u8>) -> result<list<u8>, error>; }
  resource topic  { publish: func(msg: list<u8>) -> result<_, error>;
                    subscribe: func() -> stream; }
  resource memory { apply: func(op: list<u8>) -> result<_, error>;   // CRDT delta
                    snapshot: func() -> list<u8>; }
  resource wallet { balance: func() -> u64;
                    spend: func(to: peer, amt: u64) -> result<proof, error>; } // OpenMesh
  record quota { bytes: u64, flops: u64, secs: u64 }
  type peer = list<u8>;  type proof = list<u8>;
  variant error { denied, revoked, quota-exceeded, partitioned, internal(string) }
}

world agent {
  import caps;
  import wasi:io/streams@0.2.0;
  export run: func(args: list<string>) -> result<_, string>;
}
```

A worker that was handed only a `vram` handle scoped to 2 GiB literally has no name by which to reach anything else. No ambient `open`, no global filesystem, no network except via granted `topic`/`gpu` handles.

---

## 8. Error / Status Codes (cross-cutting)

| Code | Meaning | Raised by |
|---|---|---|
| `DENIED` | no capability for the action | 00, 04 |
| `REVOKED` | capability present but revoked | 00, 07 |
| `QUOTA_EXCEEDED` | caveat violated (bytes/flops/secs) | 00, 03, 04 |
| `PARTITIONED` | target unreachable; state will CRDT-merge later | 01, 02 |
| `THERMAL_SHED` | work shed due to thermal/throttle | 06, 09 |
| `SLEEP_IMMINENT` | node handing back capabilities before sleep | 09 |
| `PROOF_INVALID` | settlement proof failed verification | 05 |
| `ATTEST_FAILED` | TEE attestation rejected (Sealed) | 00, 07 |

---

## 9. Profile Config (JSON Schema)

Full schema in [ARCHITECTURE.md §3.7](../../ARCHITECTURE.md). Boot validation: `sealed ⇒ economy.enabled=false ∧ attestation.enabled=true`. Invalid combinations abort startup with `ATTEST_FAILED`/config error.
