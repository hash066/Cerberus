# Vertical 03 — Compute Orchestration (Sharding · WASM · Promise Pipelining)

> **Workstream A — Core Compute & Cognition Plane.** Depends on: 00 (caps/WIT host), 04 (data-plane endpoints), 06 (placement). Stub until integration: data-plane transport, synthetic placement. See [docs/workstreams.md](../workstreams.md).

> Owner archetype: **The AI Orchestrator.** Conforms to [ARCHITECTURE.md](../../ARCHITECTURE.md).

> [!IMPORTANT]
> **This is a DESIGN document, not a status report.** Read the boxes below as the
> intended architecture. What exists in the tree today is narrower, and
> [README.md](../../README.md) is the authority on it:
>
> - **There is no MLX backend.** The `mlx` and `llamacpp` "backends" this doc's
>   diagrams imply were mock transforms wearing real engines' names; they were
>   **deleted** in `72a0f4a`, not repaired. See
>   [11-pipeline-layer-range-reference.md](11-pipeline-layer-range-reference.md).
>   Do not describe Cerberus as "supporting MLX".
> - **There is no tinygrad integration.** The real GPU backend is wgpu, off by
>   default; stock binaries honestly report `backend: cpu-software`.
> - **No LLM runs through Cerberus.** The pipeline shards a 4×4 MLP **fixture**.
>   Real llama.cpp integration lives in `daemon/llama` — written, unit-tested,
>   and imported by no binary yet.
> - **Tensor (intra-layer) parallelism is not implemented.** Pipeline
>   (layer-group) placement is. `contract.ShardTensor` is declared and set by no
>   code; `TPRank`/`TPWorld` are plumbed through the mesh wire
>   (`daemon/mesh/compute.go:114`) as pass-through fields that nothing acts on.
> - **Promise pipelining is partial**, and the trust bootstrap is the
>   self-issuer model rather than full CapTP.

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
- **WASM component execution + pipeline sharding + promise pipelining: Shippable** (exo proves sharding; Wasmtime proves portable exec). *Tensor sharding is design-only — see the banner at the top.*
- **Real-time inference of very large models over Wi-Fi: Frontier** — fundamentally latency-bound. Marginal over Wi-Fi. **The previous claim that this is "viable today over Thunderbolt/RDMA" was aspiration, not measurement, and is withdrawn:** Cerberus has no RDMA transport, `EndpointRDMA` is a constant no code path uses, and `daemon/hostinfo/hostinfo_test.go:63` fails the build if any code claims RDMA at all. Thunderbolt *link classification* is real (`iface_linux.go:78` reads the kernel driver name); classifying a link is not transporting over it.
- **The measured reality, so nobody has to rediscover it:** tested out-of-tree with upstream llama.cpp on one machine (single run, not through Cerberus), splitting a model over `ggml-rpc` ran ~45 tok/s against ~396 tok/s for a single node holding the whole model — **~9× slower**. Activations cross the network at every layer boundary; that cost is structural, not a tuning bug. The honest value proposition for distributed inference is therefore *"run a model that fits on no single machine you own"* — **never** *"go faster"*. Do not let a design doc imply otherwise.

Open questions: optimal micro-batch size vs link RTT; whether to expose a WASI-NN path for accel instead of bespoke `gpu` capability.
