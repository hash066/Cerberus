# mlx (Python MLX inference sidecar)

Phase 1 of the MLX backend: a Python sidecar (`sidecar.py`) spawned by the Go
daemon, speaking one JSON object per line over stdin/stdout. Go side:
`daemon/inference/mlx_sidecar.go` (process + protocol client) and
`daemon/inference/mlx_engine.go` (backend selection, mock fallback, real-model
surfaces). The script is `go:embed`-ded into the daemon binary and written to
a temp file at spawn, so a shipped binary needs no source checkout.

## Dependencies

- macOS on **Apple Silicon** (the `mlx` pip package supports nothing else)
- Python 3.9+ on `PATH` (or `CERBERUS_MLX_PYTHON=/path/to/python`)
- `pip install mlx mlx-lm`
- First real-model op downloads weights from Hugging Face
  (default `mlx-community/Llama-3.2-1B-Instruct-4bit`, ~700 MB; override with
  `CERBERUS_MLX_MODEL=<hf-id-or-local-path>`)

## What is REAL

- The sidecar process, the JSON-lines IPC, and the layer-range protocol
  (`LayerLo`/`LayerHi` from `contract.Shard`, activation in, activation out).
- `forward_splitmlp`: the 4-dim split-MLP pipeline fixture computed with real
  mlx arrays, weights identical to `daemon/system/splitmlp.go` — so pipeline
  output is verifiable against the Go fixture (used when `CERBERUS_MLX_MODEL`
  is unset).
- `forward_layers`: real model layer-range forward on byte activations plus
  shape metadata — token ids (i32) at layer 0, f32 hidden states between
  shards. Used by `inference.ForwardActivation` when `CERBERUS_MLX_MODEL` is
  set and the sidecar has mlx-lm.
- `forward`: one real full-sequence forward pass over model layers
  `[layer_lo, layer_hi]` inclusive — token ids in at layer 0, hidden states
  between shards, last-position logits + top-k next tokens at the last layer.
  Exposed in Go as `inference.MLXForwardPass`.
- `generate`: real single-node token generation via `mlx_lm.generate`.
  Exposed in Go as `inference.MLXGenerate`.

## What is NOT (honest limitations)

- **macOS/Apple Silicon only.** On every other platform (including this
  repo's Windows/Linux CI) the pipeline `mlx` backend falls back to the same
  split-MLP weights in pure Go and reports **`mlx-mock`** — the backend
  string never lies. `MLXGenerate`/`MLXForwardPass` return a clear error.
- **The distributed pipeline carries split-MLP activations by default.**
  Without `CERBERUS_MLX_MODEL` the v0.1 pipeline wire format is a fixed 4-dim
  f32 vector (`inference.ActivationDim`). With `CERBERUS_MLX_MODEL` set,
  `forward_layers` accepts/returns variable-size hidden states via
  `inference.Activation` (shape + dtype + payload bytes).
- **No KV cache / incremental decode** in `forward` — it is one full-sequence
  pass. `generate` uses mlx_lm's own (cached) decode loop.
- **One sidecar per daemon, no restart-on-crash.** The sidecar is probed once
  per process; if it dies or a call times out it is killed and the mlx
  backend stays degraded until the daemon restarts.
- **No benchmark numbers are claimed anywhere.** Measure on your own machine.

## Selecting the backend

```bash
cerberusd -pipeline-backend mlx     # or CERBERUS_PIPELINE_BACKEND=mlx
cerberus pipeline-run               # backend field reports mlx or mlx-mock
```

`CERBERUS_MLX_MOCK=1` forces the mock path even on a capable machine
(useful for deterministic tests).

## Single-node smoke test (Apple Silicon only)

```bash
pip install mlx mlx-lm
export CERBERUS_MLX_MODEL=mlx-community/Llama-3.2-1B-Instruct-4bit
go test ./daemon/inference/ -run TestMLXRealForwardPass -v   # real forward pass, top-k logits
go test ./daemon/inference/ -run TestMLXRealForwardLayers -v # per-layer shard forward
go test ./daemon/inference/ -run TestMLXSidecarProcessProtocol -v
```

Both tests skip (with the reason) anywhere the sidecar cannot genuinely run.
