# Vertical 01 — Mesh Fabric & Transport

> **Workstream B — Connectivity & Trust Fabric.** Depends on: 00 (cap-gated topics). Stub until integration: capability mechanism. See [docs/workstreams.md](../workstreams.md).

> Owner archetype: **The Network/State Engineer.** Conforms to [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Purpose & Responsibilities
- Provide zero-config peer discovery and a resilient, masterless transport across chaotic Wi-Fi and multi-site links.
- Carry the **control plane** (telemetry, CRDT deltas, CapTP frames, scheduling) — *not* bulk data (that is the QUIC/RDMA data plane, see [04](04-9p-peripheral-virt.md)).
- Provide **Erlang/OTP-style supervision trees** so daemon subsystems and peer sessions fail and recover predictably.

## 2. Position in the System
- **Control plane**, lowest software layer above the OS network stack.
- Upstream of: everything (it is the transport). Downstream of: OCap (01 topics are capability-gated).

## 3. Detailed Architecture — the hybrid fabric
The defining decision of Volume II: **Zenoh intra-site, libp2p inter-site.** Rationale recorded in [ideadumpp2.md §2](../research/ideadumpp2.md) — `braindomp.md` shows `exo` removed libp2p because its Kademlia DHT melted on local Wi-Fi; Zenoh's data-centric pub/sub (≈4-byte overhead, automatic mesh routing) is the right intra-site fabric. libp2p is retained only for what it is genuinely best at: NAT traversal and relay upgrade across sites.

```
   ┌──────────────────────── SITE A (LAN) ────────────────────────┐
   │   nodeA1 ⇄ nodeA2 ⇄ nodeA3     (Zenoh data-centric pub/sub)   │
   │     keys: cerberus/telemetry/* , cerberus/crdt/* , captp/*    │
   │   auto-router: A2 relays A1↔A3 across a Wi-Fi blind spot      │
   └───────────────────────────────┬──────────────────────────────┘
                                    │  inter-site bridge
                       libp2p: DCUtR hole-punch + Gossipsub v1.1
                                    │
   ┌───────────────────────────────┴──────────────────────────────┐
   │                         SITE B (LAN, Zenoh)                    │
   └───────────────────────────────────────────────────────────────┘
```

**Discovery:** mDNS (`_cerberus._tcp`, multicast `224.0.0.251` / `ff02::fb`) for the LAN; libp2p bootstrap + DCUtR for WAN. On discovery, peers complete a QUIC handshake with mTLS over ephemeral keys bound to the cryptographic PeerID (Ed25519 public key).

**Supervision trees (OTP-style, Go):**
```
root supervisor (one-for-all)
├── mesh supervisor (rest-for-one)
│   ├── zenoh session        (restart: permanent)
│   ├── libp2p host          (restart: permanent)
│   └── per-peer session     (restart: transient, backoff)
├── telemetry worker (08)    (restart: permanent)
├── scheduler (06)           (restart: permanent)
└── ninep server (04)        (restart: permanent)
```
Restart strategies mirror Erlang/OTP: `permanent` always restarts, `transient` restarts only on abnormal exit, with exponential backoff and a max-restart-intensity circuit breaker. This is the formalization of Volume I's "lid-drop" recovery.

**Ephemeral coordination:** there is no permanent master. Where a short-lived single-writer lease is genuinely needed (e.g., who owns a scheduling epoch), a **Raft** group is elected for that lease only (`hashicorp/raft`), tuned for sub-200 ms failover, and dissolved after. Steady-state coordination is CRDT-emergent ([02](02-distributed-state-crdts.md)).

## 4. Data Structures / Wire Formats
- Zenoh key space: `cerberus/<site>/telemetry/<peer>`, `cerberus/<site>/crdt/<doc>`, `cerberus/<site>/captp/<peer>`, `cerberus/<site>/sched/*`.
- Telemetry: [schemas §2](../schemas/schemas.md). CRDT ops: [schemas §3](../schemas/schemas.md). CapTP frames: [00](00-ocap-security-kernel.md).
- libp2p: Gossipsub v1.1 topics for cross-site mesh membership + peer scoring; DCUtR for hole-punching.

## 5. Interfaces / APIs (Go)
```go
type Fabric interface {
    Publish(ctx context.Context, key string, msg []byte, cap CapRef) error
    Subscribe(ctx context.Context, keyExpr string, cap CapRef) (<-chan Sample, error)
    Dial(peer PeerID) (Session, error)          // QUIC, mTLS
    Peers() []PeerInfo
}
type Supervisor interface {
    Spawn(child ChildSpec) error                 // Restart: Permanent|Transient|Temporary
    Strategy() Strategy                          // OneForOne|OneForAll|RestForOne
}
```
Every `Publish`/`Subscribe` requires a `topic` capability; the fabric calls `cap_verify` ([00](00-ocap-security-kernel.md)) before binding.

## 6. Tech Stack
| Concern | Choice | Why |
|---|---|---|
| Intra-site fabric | **Zenoh** | data-centric, ~4-byte overhead, auto mesh routing/healing, robotics-grade; avoids exo's DHT meltdown |
| Inter-site fabric | **go-libp2p** (DCUtR, Gossipsub v1.1) | best-in-class NAT traversal + relay; Sybil-resistant peer scoring |
| Transport | **QUIC** (`quic-go`), mTLS | multiplexed streams, encrypted by default, 1-RTT handshake ([not 0-RTT](#0-rtt-is-deliberately-off)) |
| Discovery | mDNS + libp2p bootstrap | zero-config LAN + WAN; the mDNS service tag is scoped by site (`cerberus-<site>`) so different sites on one LAN do not cross-discover |
| Ephemeral leases | `hashicorp/raft` | sub-200 ms leader election, lease-scoped only |
| Supervision | custom Go OTP-style supervisor | predictable fault tolerance |

### 0-RTT is deliberately off

This table previously claimed **0-RTT**. Nothing implemented it — `Allow0RTT` was
never set on either the mesh or the data-plane QUIC config — so the claim was
simply wrong, and it is corrected here rather than implemented.

It is corrected rather than implemented on purpose. 0-RTT data is, by
construction, **replayable**: an attacker who captures a 0-RTT first flight can
re-send it, and the server cannot distinguish the replay from the original
(RFC 9001 §9.2). The data plane's first flight is the transfer header, and the
transfer header carries the **capability** that authorizes the transfer
(`daemon/dataplane/frame.go`). Accepting a replayable capability presentation is
precisely the property a capability system must not have — it would let a
captured grant be re-played against its quota. Per CLAUDE.md golden rule 5
("capabilities, not identities — no ambient authority") that trade is not
available to us for a one-RTT saving on a LAN where RTT is ~1ms.

Wiring it later is possible but is not a config flip: the header would need an
anti-replay nonce the server tracks, and only then could `Allow0RTT` be set on
the listener with a `tls.ClientSessionCache` on the dialer. Until that exists,
this stays off and undocumented as a feature.

**Congestion control** is Cubic, not BBR: `quic-go` ships Cubic only and exposes
no pluggable CC hook. Flow-control windows are tuned instead
(`daemon/dataplane/quicconf.go`), which is measured at +17.9% on a high-RTT path
and a no-op on a LAN — see that file for the numbers and the method.

## 7. Security Model
- All topics are capability-gated; no node can subscribe to telemetry/CRDT streams without a `topic` capability.
- Gossipsub v1.1 **peer scoring + Sybil resistance** prevents telemetry spam and mesh poisoning across sites.
- mTLS binds sessions to PeerIDs; ephemeral keys rotate per session.

## 8. Open Mesh vs Sealed
- **OpenMesh:** inter-site libp2p enabled for cross-org meshing; permissive bootstrap.
- **Sealed:** inter-site restricted to org-CA-attested peers; cross-org topics disabled; mDNS scope may be pinned to a VLAN.

## 9. Failure Modes & Mitigations
| Failure | Mitigation |
|---|---|
| Wi-Fi blind spot (A1 can't see A3) | Zenoh auto-router uses A2 as relay; no manual routing |
| DHT latency spikes (the exo problem) | **avoided by design** — no global DHT intra-site; Zenoh pub/sub instead |
| Peer flapping | transient restart + exponential backoff + max-intensity breaker |
| Coordinator loss | Raft re-election <200 ms; in-flight compute not cancelled |
| NAT/AP isolation across sites | libp2p DCUtR hole-punch, relay fallback |

## 10. Verdict
**Shippable.** Both Zenoh and libp2p are mature; the hybrid is the considered correction of Volume I. Open question: exact site-boundary detection heuristic (subnet vs RTT-cluster) for choosing Zenoh-vs-libp2p path automatically.
