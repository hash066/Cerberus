# Vertical 03 — Compute Orchestration (Sharding · WASM · Promise Pipelining)

> Owner archetype: **The AI Orchestrator.** Conforms to [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Purpose & Responsibilities
- Execute agent/worker code **portably** across ARM/x86/Metal/Vulkan via **WASM components** + native acceleration backends.
- Split models that exceed a single node: **pipeline parallelism** (layer groups per node) and **tensor parallelism** (intra-layer shards).
- Mask Wi-Fi latency with **promise pipelining** (CapTP) so a round-trip is not paid per dependency hop.

## 2. Position in the System
- **Execution layer**, straddling control (task dispatch via 01) and data (activations via QUIC/RDMA, see [04](04-9p-peripheral-virt.md)).
- Upstream: scheduler (06) decides placement; OCap (00) grants the worker its capabilities. Downstream: economy (05) settles completed work (OpenMesh).

## 3. Detailed Architecture
```
   ComputeTask (component CID + shard + caps + promise deps)
        │ scheduler(06) places shards
        ▼
   ┌───────── node A ─────────┐   ┌───────── node B ─────────┐   ┌──── node C ────┐
   │ Wasmtime host            │   │ Wasmtime host            │   │ Wasmtime host   │
   │ component: layers 1–20   │──►│ component: layers 21–40  │──►│ layers 41–60    │
   │ MLX backend (Apple)      │   │ wgpu+tinygrad (CUDA-free)│   │ ...             │
   └──────────┬───────────────┘   └──────────┬───────────────┘   └────────┬───────┘
   activations over QUIC zero-copy / RDMA-over-TB (DATA PLANE — not 9P, not control)
              └───────── promise pipelining: B starts on A's promise before bytes land ──────────┘
```
- **WASM Component Model workers:** general compute compiles to **components** (not bare modules), so complex inputs/outputs (records, lists, strings) cross node boundaries through typed WIT interfaces with no raw memory exposure ([schemas §7](../schemas/schemas.md)). Cranelift JITs to the host ISA at load.
- **AI acceleration:** the heavy linear algebra does **not** run inside WASM (too slow). The WASM component is the *orchestrator/glue*; it calls granted `gpu` capabilities that dispatch to **MLX** (Apple unified memory + ANE) or **wgpu + tinygrad** (Vulkan/Metal/DX shaders, no CUDA dependency). Tensors live in the data plane.
- **Sharding:** `Shard` descriptor ([schemas §3.4](../schemas/schemas.md)) carries `PIPELINE` (layer_lo..layer_hi) or `TENSOR` (tp_rank/tp_world). Layer partitioning follows live VRAM telemetry (06).
- **Promise pipelining:** following CapTP, a downstream shard is dispatched with a *promise* to the upstream result. Node C can enqueue work against B's promise before A→B activations arrive, collapsing serial Wi-Fi round-trips (this is the GPipe micro-batching idea generalized to capability promises).

## 4. Data Structures / Wire Formats
- `ComputeTask`, `Shard`, `Promise` — [schemas §3.4](../schemas/schemas.md).
- Activation frames: length-prefixed zstd-compressed tensors over QUIC streams (dictionary tuned to link quality, per Volume I §3); zero-copy where the runtime allows.
- Component artifacts addressed by **IPLD CID** and fetched peer-to-peer ([04](04-9p-peripheral-virt.md), [05](05-eutxo-open-mesh-economy.md)).

## 5. Interfaces / APIs
WIT world `agent` ([schemas §7](../schemas/schemas.md)); host exposes `gpu.submit`, `vram`, `memory`, `topic`.
Go (dispatch side):
```go
type Executor interface {
    Dispatch(t ComputeTask) (Promise, error)        // returns a resolvable promise
    Resolve(p Promise) ([]byte, error)
}
```

## 6. Tech Stack
| Concern | Choice | Why |
|---|---|---|
| WASM runtime | **Wasmtime + Cranelift**, Component Model, WASI P2 | portable, sandboxed, typed cross-node values, JIT to host ISA |
| Apple accel | **MLX** | unified memory + ANE; ideal for Apple Silicon laptops |
| PC/Linux accel | **wgpu + tinygrad** | one WGSL/shader path across Vulkan/Metal/DX; **no CUDA install** |
| Transport (data) | QUIC zero-copy / RDMA-over-TB ([04](04-9p-peripheral-virt.md)) | activations at link speed |
| Pipelining | CapTP promises ([00](00-ocap-security-kernel.md)) | hide per-hop RTT |

## 7. Security Model
- A worker component holds **only** the capabilities the scheduler granted it (e.g., one `gpu` handle + one `topic`). No ambient filesystem/network.
- Quota caveats (`max_flops`, `max_bytes`, `max_secs`) are enforced by the kernel; exceeding them yields `QUOTA_EXCEEDED`.
- Inputs/outputs are content-addressed (CID), so a worker cannot be fed an unverifiable payload.

## 8. Open Mesh vs Sealed
- **OpenMesh:** completed tasks emit a settlement claim ([05](05-eutxo-open-mesh-economy.md)); optional zk-WASM proof of execution.
- **Sealed:** no settlement; execution confined to org-attested nodes; optionally requires TEE-attested workers.

## 9. Failure Modes & Mitigations
| Failure | Mitigation |
|---|---|
| Wi-Fi jitter stalls pipeline | promise pipelining + predictive prefetch of next shard's inputs |
| Node fails mid-shard | scheduler (06) re-routes to hot standby; promise rejects → retried |
| WASM too slow for matmul | heavy math offloaded to MLX/wgpu via `gpu` capability, not run in WASM |
| Model too big for any node | pipeline + tensor sharding across telemetry-fit layer groups |

## 10. Verdict
- **WASM component execution + tensor/pipeline sharding + promise pipelining: Shippable** (exo proves sharding; Wasmtime proves portable exec).
- **Real-time inference of very large models over Wi-Fi: Frontier** — fundamentally latency-bound; viable today over Thunderbolt/RDMA, marginal over Wi-Fi. Honest margin.

Open questions: optimal micro-batch size vs link RTT; whether to expose a WASI-NN path for accel instead of bespoke `gpu` capability.
