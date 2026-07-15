# split-mlp (pipeline demo fixture)

This directory documents the **4-layer MLP pipeline fixture** used by the
layer-split demo. The executable implementation lives in Go (CPU, honest
`cpu-software` backend):

- `daemon/system/splitmlp.go` — 4 fully-connected layers, dim=4, ReLU on hidden layers
- Shards use `contract.ShardPipeline` with `LayerLo` / `LayerHi` ranges
- Default demo input: `[1, 0, 0, 0]`

There is **no WASM component here yet** — pipeline shards run via the Go fixture
through `PipelineRunner` and signed mesh compute (`daemon/mesh/compute.go`).
A future WASM port would compile a tiny per-shard component; v0.1 prioritizes
end-to-end orchestration (placement → compute → dataplane activation handoff).

## Default shard split

| Shard | Layers |
|-------|--------|
| 0     | 0–1    |
| 1     | 2–3    |

Run locally:

```bash
cerberus pipeline-run
```

Two-node harness:

```bash
go run ./test/pipeline_e2e
```
