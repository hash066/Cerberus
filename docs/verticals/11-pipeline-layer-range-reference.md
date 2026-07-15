# 11 — Layer-range forward: a Phase-2 reference (NOT a backend)

> **STATUS: REFERENCE MATERIAL ONLY. Nothing described here is wired into Cerberus.**
> There is no MLX backend, no llama.cpp layer-range backend, and no Python sidecar
> in this repo. This file exists so that the one genuinely correct idea from the
> deleted `daemon/inference/mlx/sidecar.py` is not lost. Do not cite it as a
> capability. Do not describe Cerberus as "supporting MLX".

## Why this file exists

Cerberus v0.1 shipped a family of "inference backends" (`llamacpp`, `mlx`) that were
mock transforms wearing a real backend's name. They were deleted in Lane L / L0
(see the commit that added this file). The audit that motivated the deletion is
summarised in [docs/verticals/03-compute-orchestration.md](03-compute-orchestration.md)
and the honest current state is in [ARCHITECTURE.md §8](../../ARCHITECTURE.md).

Exactly one fragment of that deleted code was real: the MLX sidecar's
`_run_layer_range`. It was the only function in the entire repository that actually
applied a contiguous range of real transformer layers to a real hidden state. It is
preserved verbatim below.

## The reference fragment

From the deleted `daemon/inference/mlx/sidecar.py` (Python, mlx-lm):

```python
def _run_layer_range(model, inner, lo, hi, h):
    """Apply transformer layers [lo, hi] inclusive to hidden state h."""
    mx = import_mlx()
    layers = inner.layers
    mask = _causal_mask(mx, h) if h.shape[1] > 1 else None
    for layer in layers[lo : hi + 1]:
        h = layer(h, mask, None)
    return h
```

Supporting fragment — building the layer-0 hidden state from token ids:

```python
def _layer0_hidden(tokenizer, inner, req):
    """Build initial hidden states for layer 0 from tokens, prompt, or bytes."""
    mx = import_mlx()
    tokens = req.get("tokens")
    if not tokens:
        prompt = req.get("prompt") or DEFAULT_PROMPT
        tokens = tokenizer.encode(prompt)
    return inner.embed_tokens(mx.array([tokens]))
```

## What is actually correct about it

- **The slice is the whole idea.** `layers[lo : hi + 1]` over a hidden state `h`
  is genuinely what a pipeline-parallel stage does. `h` in, `h` out, same shape.
- **The causal mask is conditioned on sequence length** (`h.shape[1] > 1`), which is
  the right distinction between a prefill (needs a mask) and a single-token decode
  step (does not).
- **Layer 0 is special** and the rest are not: only the first stage embeds tokens.

## What it does NOT solve (why this is Phase 2, not Phase 1)

This fragment is a stateless forward over one hidden state. Real distributed decoding
needs all of the following, none of which existed:

1. **KV-cache residency.** `layer(h, mask, None)` passes `None` as the cache. That is
   correct *only* for a single prefill of the full prompt. Token-by-token decode needs
   a per-layer KV cache that lives on the node owning that layer range, across many
   calls, keyed by sequence. Shipping the cache between nodes each token would cost
   far more than the activation.
2. **Sampling.** No logits head, no sampler, no stop conditions, no detokenizer.
3. **Tokenizer parity.** The tokenizer must be byte-identical across nodes; a
   mismatch silently produces garbage rather than an error.
4. **Batching / continuous batching**, without which throughput is hopeless.
5. **Numerics.** Cross-node accumulation order and dtype must be pinned or results
   drift between runs and between machines.

Items 1–4 are precisely what `llama-server` already implements and what Lane L
chose to reuse rather than reimplement in Go. **The current real path
(`daemon/llama/`) does not use this fragment at all**: llama.cpp's own RPC backend
owns the layer split, the KV cache, and sampling, and Cerberus contributes the
capability-gated transport underneath it. See `daemon/llama/doc.go`.

## If you pick this up in Phase 2

The reason to revive a Cerberus-native layer-range forward is *not* performance —
it is to run stages inside the WASM/ocap sandbox with per-stage capability
attenuation, which llama.cpp's RPC backend cannot do (it is a trusted C++ peer;
see the CVE note in `daemon/llama/doc.go`). That is a security argument, not a
speed argument, and it only pays for itself once items 1–5 above are solved.

Start by reading `daemon/inference/pipeline_activation.go` — the activation wire
format and the shard layer-range plumbing there are real and still in use by the
`splitmlp` test fixture.
