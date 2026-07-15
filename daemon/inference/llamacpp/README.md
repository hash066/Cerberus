# llamacpp (llama.cpp inference helper)

Phase 1 of the llamacpp pipeline backend: a JSON-lines helper spawned by the Go
daemon. Go side: `daemon/inference/llamacpp_sidecar.go` (process + protocol
client) and `daemon/inference/llamacpp_engine.go` (backend selection, mock
fallback). The Python script is `go:embed`-ded into the daemon binary and
written to a temp file at spawn, so a shipped binary needs no source checkout.

An optional native helper binary (`cerberus-llama-forward`, same protocol) can
be used instead via `CERBERUS_LLAMA_HELPER` — see `forward.cpp` + `CMakeLists.txt`.

## Dependencies (real forward)

- Windows or Linux build of Cerberus (`llamacpp_subprocess.go`)
- Python 3.9+ on `PATH` (or `CERBERUS_LLAMA_PYTHON`)
- `pip install llama-cpp-python` (pulls/builds a llama.cpp backend)
- A local GGUF model path via `CERBERUS_LLAMA_MODEL` or `LLAMA_MODEL`
- Single-node token generation still uses `llama-cli` via `Complete()` and
  `CERBERUS_LLAMA_CLI` when set

## What is REAL

- The helper process and JSON-lines IPC (`forward` op: activation in, activation out).
- `forward` runs a real GGUF decode with llama.cpp weights when the sidecar
  (or native helper) and model path are available.
- `Complete()` remains the honest single-node text-generation path via
  `llama-cli` subprocess.

## What is NOT (honest limitations)

- **No stable llama.cpp public API for arbitrary mid-layer injection.** v0.1 maps
  demo shard layers `[0..3]` onto the model layer count proportionally and
  blends 4-dim pipeline activations into the first `n_embd` floats at layer 0.
  Full hidden-state shard chaining requires a future wire-format lift (see
  `contract/go/activation.go` and MLX README).
- **One decode per `forward` request** — no KV-cache reuse across pipeline stages.
- **Eval-callback capture is best-effort.** When unavailable, the helper falls
  back to final embeddings and documents that in errors/logs.
- **Mock fallback** when Python/llama-cpp-python/model path is missing; reported
  backend stays `llamacpp-mock` (never lies).

## Selecting the backend

```bash
cerberusd -pipeline-backend llamacpp
# or CERBERUS_PIPELINE_BACKEND=llamacpp
```

`CERBERUS_LLAMACPP_MOCK=1` forces the deterministic mock path.

## Smoke tests

```bash
go test ./daemon/inference/ -run TestLlamacpp -v
```

Real-model tests skip when `CERBERUS_LLAMA_MODEL` is unset or the helper cannot
load llama-cpp-python.

## Optional native helper build

```bash
cd daemon/inference/llamacpp
cmake -B build -DLLAMA_CPP_DIR=/path/to/llama.cpp
cmake --build build
export CERBERUS_LLAMA_HELPER=$PWD/build/cerberus-llama-forward
```

The C++ helper speaks the same JSON-lines protocol as `sidecar.py`.
