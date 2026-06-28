# Vertical 04 — 9P Peripheral Virtualization

> Owner archetype: **The Kernel Hacker.** Conforms to [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Purpose & Responsibilities
- Expose every remote peripheral — GPU, VRAM, audio, storage — as a **capability-addressed file namespace** using the Plan 9 **9P2000.L** protocol, mounted locally via FUSE (Unix) / WinFsp (Windows).
- **Critical boundary:** 9P is the **control plane** — it enumerates, describes, and *grants* devices. It is **not** the data plane. Tensor/VRAM bytes never traverse 9P `read`/`write`; opening a control file returns a **data-plane endpoint** (QUIC stream / RDMA handle).
- Provide a distributed filesystem (JuiceFS-style metadata/data split + Reed-Solomon + IPLD) and network audio (AES67/ROC).

## 2. Position in the System
- **Control plane** (9P namespace) + **data plane** (QUIC/RDMA) clearly separated.
- Upstream: OCap (00) gates every `walk`/`open`. Downstream: compute (03) consumes GPU/VRAM endpoints; economy (05) consumes IPLD blocks.

## 3. Detailed Architecture
```
   Windows rig (Explorer)         macOS (Finder)          Linux (VFS)
        │ WinFsp+cgofuse              │ go-fuse                │ go-fuse
        └──────────────┬─────────────┴───────────┬────────────┘
                       ▼                          ▼
              ┌──────────────────  9P2000.L server (Go)  ──────────────────┐
              │  namespace: /cer/dev/{gpu,vram,audio}  /cer/fs  /cer/proc   │
              │  every Twalk/Topen → cap_verify (00)                       │
              └───────────────┬───────────────────────────────────────────┘
        open /cer/dev/vram/B/0/ctl  ─returns→  data-plane endpoint  ──────────────┐
                                                                                  ▼
                                              QUIC zero-copy  /  RDMA-over-Thunderbolt
                                              (Mac↔Mac: write into peer RAM, skip OS net stack)
```
**Why a Windows rig can "mount a MacBook's GPU":** the Mac publishes `/cer/dev/gpu/<mac>/0`; the Windows node, holding a `gpu` capability, walks to it through WinFsp→9P and sees a device directory. Opening `ctl` returns an endpoint over which compute (03) streams work. The GPU appears as a local path; the bytes flow over the data plane, not the file read.

**Distributed filesystem** (`/cer/fs`): JuiceFS-style split — metadata (names, sizes, locks) in a fast transactional store; file content sliced into 64 MiB chunks → 4 MiB blocks, **Reed-Solomon** erasure-coded, content-addressed as **IPLD** CIDs, scattered across peer free space. Concurrent writes use metadata transaction locks.

**Audio** (`/cer/dev/audio`): PipeWire (Linux) / CoreAudio (macOS) capture nodes; frames packed to **AES67/ROC** over QUIC with a delay-locked loop resampler to kill clock drift (Volume I §4 fix: ROC instead of raw NTP).

## 4. Data Structures / Wire Formats
- 9P2000.L messages (Tversion/Tattach/Twalk/Topen/Tread/Twrite/Tclunk) over QUIC.
- Namespace + op→capability table: [schemas §5](../schemas/schemas.md).
- `ctl` open response: `{kind: "quic"|"rdma", endpoint, stream_id|rdma_key, quota}`.
- FS block: `{cid, rs_shards: [k+m], placement: [peer...]}` (IPLD dag-cbor).
- Audio: ROC sender/receiver descriptors (AES67 SDP-like).

## 5. Interfaces / APIs (Go)
```go
type NineP interface {
    Walk(fid Fid, names []string, cap CapRef) (Qid, error)
    Open(fid Fid, mode uint8, cap CapRef) (DataEndpoint, error)  // ctl → endpoint, never bytes
    Read(fid Fid, off uint64, n uint32) ([]byte, error)          // only for /info, /fs metadata
}
type DistFS interface { Put(r io.Reader) (CID, error); Get(cid CID) (io.ReadCloser, error) }
```

## 6. Tech Stack
| Concern | Choice | Why |
|---|---|---|
| Protocol | **9P2000.L over QUIC** | clean "everything is a file" control plane; capability-friendly walk/open |
| FUSE (Unix) | **hanwen/go-fuse** | mature native-Go bindings, async inode handling |
| Windows mount | **WinFsp + cgofuse** | Windows lacks native FUSE; single code path across Explorer/Finder |
| Data plane | QUIC zero-copy; **RDMA-over-Thunderbolt** | activations/VRAM at link speed; TB5 skips OS net stack |
| Distributed FS | JuiceFS-style split, **klauspost/reedsolomon**, **IPLD** | metadata/data decoupling, erasure durability, content addressing |
| Audio | PipeWire/CoreAudio + **ROC/AES67** | phase-aligned, packet-loss-concealed network audio |

## 7. Security Model
- **Every `walk` and `open` is capability-checked.** Mounting a remote GPU requires a `gpu`/`vram` capability; a 2 GiB VRAM grant is a capability whose `quota.bytes = 2 GiB` caveat the 9P server enforces.
- The namespace is **per-principal**: a node only sees device files for which it holds (or can be delegated) capabilities — Plan 9 per-process namespaces realized as capability-filtered views.
- Data-plane endpoints are single-use, capability-bound, and expire with the capability.

## 8. Open Mesh vs Sealed
- **OpenMesh:** cross-org device sharing permitted via delegated capabilities; FS blocks may be served to paying peers (settled in [05](05-eutxo-open-mesh-economy.md)).
- **Sealed:** device namespace org-scoped; FS encryption-at-rest + audit; no cross-org block serving.

## 9. Failure Modes & Mitigations
| Failure | Mitigation |
|---|---|
| Naive "VRAM as a file you read()" | **excluded by design** — `ctl` returns an endpoint; data plane carries bytes |
| Node holding FS blocks drops | Reed-Solomon `k+m` redundancy survives `m` losses; re-replicate on telemetry change |
| Windows can't mount `/mnt/cerberus` | WinFsp kernel proxy + cgofuse bridge |
| Audio clock drift / echo | ROC delay-locked-loop resampling, AES67 timing |
| 9P chattiness over Wi-Fi | walk batching, QUIC 0-RTT, aggressive attribute caching |

## 10. Verdict
- **9P as capability-addressed control plane + FUSE/WinFsp mounts + IPLD/Reed-Solomon FS: Shippable.**
- **VRAM-as-file *data* path: correctly excluded** (physically wrong over Wi-Fi).
- **RDMA-over-Thunderbolt data plane: Shippable on Mac (TB4/5), Frontier elsewhere.**

Open questions: attribute-cache coherence window for `/cer/dev/*`; whether to expose `/cer/fs` snapshots as 9P versioned dumps.
