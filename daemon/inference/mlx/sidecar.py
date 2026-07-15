#!/usr/bin/env python3
"""Cerberus MLX inference sidecar (v0.1, Phase 1).

Speaks newline-delimited JSON with the Go daemon (daemon/inference/
mlx_sidecar.go): one request object per line on stdin, exactly one response
object per line on stdout, strictly in request order. Anything else the
process wants to say goes to stderr.

Ops:
  ping             -> {"ok": true}
  info             -> platform, python/mlx availability, configured model
  forward_splitmlp -> the deterministic 4-dim split-MLP pipeline fixture
                      computed with REAL mlx arrays; weights mirror
                      daemon/system/splitmlp.go so outputs are comparable
  forward          -> REAL model layer-range forward pass [layer_lo, layer_hi]
                      inclusive: token ids in at layer 0 (or hidden states for
                      mid-pipeline shards), hidden states out; when layer_hi is
                      the last layer, also returns last-position logits and the
                      top-k next tokens
  generate         -> single-node text generation via mlx_lm.generate
  shutdown         -> {"ok": true}, then exit 0

MATURITY HONESTY (see README.md next to this file):
  - Real ops require macOS on Apple Silicon with `pip install mlx mlx-lm`.
    On any other platform the imports fail and every real op returns a clear
    {"ok": false, "error": ...} — nothing is faked.
  - `forward` runs ONE full-sequence pass: no KV cache, no incremental decode.
  - Model weights are fetched from Hugging Face on first use (network + disk).
"""

import base64
import json
import os
import platform
import struct
import sys

# Libraries (mlx_lm download progress, tokenizer warnings) must never write to
# the protocol stream. Keep the real stdout for ourselves and point sys.stdout
# at stderr for everyone else.
PROTO = sys.stdout
sys.stdout = sys.stderr

DEFAULT_MODEL = os.environ.get(
    "CERBERUS_MLX_MODEL", "mlx-community/Llama-3.2-1B-Instruct-4bit"
)
DEFAULT_PROMPT = os.environ.get("CERBERUS_MLX_PROMPT", "Hello")

SPLITMLP_LAYERS = 4
SPLITMLP_DIM = 4
TOP_K = 5

_mx = None
_mx_err = None
_models = {}  # model id -> (model, tokenizer)


def import_mlx():
    """Lazily import mlx.core; cache the failure so we report it consistently."""
    global _mx, _mx_err
    if _mx is not None or _mx_err is not None:
        return _mx
    try:
        import mlx.core as mx

        _mx = mx
    except Exception as e:  # noqa: BLE001 - report any import-time failure honestly
        _mx_err = (
            f"mlx not importable: {e} "
            "(pip install mlx; requires macOS on Apple Silicon)"
        )
    return _mx


def load_model(model_id):
    """Load (and cache) an mlx_lm model + tokenizer. Raises on failure."""
    if model_id in _models:
        return _models[model_id]
    from mlx_lm import load  # raises if mlx-lm is not installed

    model, tokenizer = load(model_id)
    _models[model_id] = (model, tokenizer)
    return model, tokenizer


def err(msg):
    return {"ok": False, "error": str(msg)}


def op_info(_req):
    mx = import_mlx()
    info = {
        "ok": True,
        "platform": sys.platform,
        "machine": platform.machine(),
        "python": platform.python_version(),
        "mlx": mx is not None,
        "model": DEFAULT_MODEL,
    }
    if mx is not None:
        info["mlx_version"] = getattr(mx, "__version__", "unknown")
    else:
        info["error"] = _mx_err
    try:
        import mlx_lm  # noqa: F401

        info["mlx_lm"] = True
    except Exception:  # noqa: BLE001
        info["mlx_lm"] = False
    return info


def op_forward_splitmlp(req):
    """The 4-dim split-MLP pipeline fixture as real mlx compute.

    Weights/biases mirror daemon/system/splitmlp.go exactly:
      w[layer][row][col] = (layer+1)*0.1 + col*0.01 + row*0.001
      b[layer][row]      = layer*0.05
    with ReLU on every layer except the last.
    """
    mx = import_mlx()
    if mx is None:
        return err(_mx_err)
    lo = int(req.get("layer_lo", 0))
    hi = int(req.get("layer_hi", 0))
    if lo > hi or hi >= SPLITMLP_LAYERS:
        return err(f"splitmlp: invalid layer range [{lo},{hi}]")
    act = req.get("activation")
    if not isinstance(act, list) or len(act) != SPLITMLP_DIM:
        return err(f"splitmlp: activation must be {SPLITMLP_DIM} floats")
    h = mx.array([float(x) for x in act], dtype=mx.float32)
    for layer in range(lo, hi + 1):
        w = mx.array(
            [
                [(layer + 1) * 0.1 + c * 0.01 + r * 0.001 for c in range(SPLITMLP_DIM)]
                for r in range(SPLITMLP_DIM)
            ],
            dtype=mx.float32,
        )
        b = mx.full((SPLITMLP_DIM,), layer * 0.05, dtype=mx.float32)
        h = w @ h + b
        if layer < SPLITMLP_LAYERS - 1:
            h = mx.maximum(h, 0)
    return {
        "ok": True,
        "activation": [float(x) for x in h.tolist()],
        "backend": "mlx",
    }


def _causal_mask(mx, h):
    """Additive causal mask for a full-sequence pass (no KV cache in v0.1)."""
    try:
        from mlx_lm.models.base import create_attention_mask

        return create_attention_mask(h, None)
    except Exception:  # noqa: BLE001 - fall back to an explicit mask
        seq = h.shape[1]
        mask = mx.triu(mx.full((seq, seq), float("-inf"), dtype=h.dtype), k=1)
        return mask


def _logits_from_hidden(model, inner, h_final):
    args = getattr(model, "args", None)
    if args is not None and getattr(args, "tie_word_embeddings", False):
        return inner.embed_tokens.as_linear(h_final)
    if hasattr(model, "lm_head"):
        return model.lm_head(h_final)
    return inner.embed_tokens.as_linear(h_final)


def _decode_activation_bytes(req):
    b64 = req.get("activation_bytes")
    if not b64:
        return None
    return base64.b64decode(b64)


def _encode_activation_bytes(raw):
    return base64.b64encode(raw).decode("ascii")


def _unpack_f32(raw):
    n = len(raw) // 4
    return struct.unpack(f"<{n}f", raw)


def _pack_f32(values):
    return struct.pack(f"<{len(values)}f", *values)


def _unpack_i32(raw):
    n = len(raw) // 4
    return list(struct.unpack(f"<{n}i", raw))


def _run_layer_range(model, inner, lo, hi, h):
    """Apply transformer layers [lo, hi] inclusive to hidden state h."""
    mx = import_mlx()
    layers = inner.layers
    mask = _causal_mask(mx, h) if h.shape[1] > 1 else None
    for layer in layers[lo : hi + 1]:
        h = layer(h, mask, None)
    return h


def _layer0_hidden(tokenizer, inner, req):
    """Build initial hidden states for layer 0 from tokens, prompt, or bytes."""
    mx = import_mlx()
    tokens = req.get("tokens")
    if not tokens:
        raw = _decode_activation_bytes(req)
        dtype = req.get("dtype", "f32")
        shape = req.get("shape") or []
        if raw and dtype == "i32" and shape:
            tokens = _unpack_i32(raw)
        elif raw and len(raw) == SPLITMLP_DIM * 4 and not shape:
            # Legacy split-MLP fixture at layer 0: tokenize the demo prompt.
            tokens = tokenizer.encode(req.get("prompt") or DEFAULT_PROMPT)
        else:
            prompt = req.get("prompt") or DEFAULT_PROMPT
            tokens = tokenizer.encode(prompt)
    return inner.embed_tokens(mx.array([tokens]))


def _mid_hidden(model, inner, req):
    """Decode mid-pipeline hidden states from activation_bytes + shape."""
    mx = import_mlx()
    hidden = req.get("hidden")
    if hidden:
        h = mx.array([hidden], dtype=mx.float32).astype(inner.embed_tokens.weight.dtype)
        return h, None
    raw = _decode_activation_bytes(req)
    shape = req.get("shape") or []
    dtype = req.get("dtype", "f32")
    if not raw or len(shape) != 2:
        return None, "forward_layers: mid-pipeline shard needs activation_bytes and shape [seq, hidden]"
    if dtype != "f32":
        return None, "forward_layers: hidden states must be dtype f32"
    seq_len, hidden = int(shape[0]), int(shape[1])
    vals = _unpack_f32(raw)
    if seq_len * hidden != len(vals):
        return None, f"forward_layers: shape {shape} vs {len(vals)} floats mismatch"
    h = mx.array(vals, dtype=mx.float32).reshape(1, seq_len, hidden)
    return h.astype(inner.embed_tokens.weight.dtype), None


def _forward_layers_core(req):
    """Shared layer-range forward for forward and forward_layers ops."""
    mx = import_mlx()
    if mx is None:
        return err(_mx_err)
    model, tokenizer = load_model(req.get("model") or DEFAULT_MODEL)
    inner = model.model
    layers = inner.layers
    n = len(layers)
    lo = int(req.get("layer_lo", 0))
    hi = int(req.get("layer_hi", n - 1))
    if lo > hi or hi >= n:
        return err(f"forward_layers: invalid layer range [{lo},{hi}] (model has {n} layers)")

    if lo == 0:
        h = _layer0_hidden(tokenizer, inner, req)
    else:
        h, msg = _mid_hidden(model, inner, req)
        if msg:
            return err(msg)

    h = _run_layer_range(model, inner, lo, hi, h)
    return model, inner, n, lo, hi, h


def op_forward_layers(req):
    """Byte-oriented layer-range forward for distributed pipeline shards."""
    out = _forward_layers_core(req)
    if isinstance(out, dict):
        return out
    model, inner, n, lo, hi, h = out
    mx = import_mlx()
    resp = {
        "ok": True,
        "backend": "mlx",
        "n_layers": n,
        "hidden_size": int(h.shape[-1]),
    }
    if hi == n - 1:
        h_final = inner.norm(h)
        logits = _logits_from_hidden(model, inner, h_final)
        last = logits[0, -1, :].astype(mx.float32)
        order = mx.argsort(last)
        top_ids = [int(t) for t in order[-TOP_K:].tolist()][::-1]
        resp["top_tokens"] = [
            {"token": t, "logit": float(last[t].item())} for t in top_ids
        ]
        flat = [float(x) for x in last.tolist()]
        resp["activation_bytes"] = _encode_activation_bytes(_pack_f32(flat))
        resp["shape"] = [len(flat)]
        resp["dtype"] = "f32"
    else:
        seq_len = int(h.shape[1])
        hidden = int(h.shape[-1])
        flat = [float(x) for x in h[0].astype(mx.float32).reshape(-1).tolist()]
        resp["activation_bytes"] = _encode_activation_bytes(_pack_f32(flat))
        resp["shape"] = [seq_len, hidden]
        resp["dtype"] = "f32"
    return resp


def op_forward(req):
    """One real forward pass over layers [layer_lo, layer_hi] inclusive."""
    out = _forward_layers_core(req)
    if isinstance(out, dict):
        return out
    model, inner, n, lo, hi, h = out
    mx = import_mlx()

    result = {"ok": True, "backend": "mlx", "n_layers": n, "hidden_size": int(h.shape[-1])}
    if hi == n - 1:
        h_final = inner.norm(h)
        logits = _logits_from_hidden(model, inner, h_final)
        last = logits[0, -1, :].astype(mx.float32)
        order = mx.argsort(last)  # ascending
        top_ids = [int(t) for t in order[-TOP_K:].tolist()][::-1]
        result["top_tokens"] = [
            {"token": t, "logit": float(last[t].item())} for t in top_ids
        ]
        if req.get("return_logits"):
            result["logits"] = [float(x) for x in last.tolist()]
    else:
        result["hidden"] = [
            [float(x) for x in row] for row in h[0].astype(mx.float32).tolist()
        ]
    return result


def op_generate(req):
    prompt = req.get("prompt")
    if not prompt:
        return err("generate: 'prompt' required")
    mx = import_mlx()
    if mx is None:
        return err(_mx_err)
    model, tokenizer = load_model(req.get("model") or DEFAULT_MODEL)
    from mlx_lm import generate

    max_tokens = int(req.get("max_tokens") or 8)
    text = generate(model, tokenizer, prompt=prompt, max_tokens=max_tokens, verbose=False)
    return {
        "ok": True,
        "text": text,
        "tokens": tokenizer.encode(text),
        "backend": "mlx",
    }


OPS = {
    "info": op_info,
    "forward_splitmlp": op_forward_splitmlp,
    "forward_layers": op_forward_layers,
    "forward": op_forward,
    "generate": op_generate,
}


def reply(obj):
    print(json.dumps(obj), file=PROTO, flush=True)


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except ValueError as e:
            reply(err(f"bad request json: {e}"))
            continue
        op = req.get("op", "")
        if op == "ping":
            reply({"ok": True})
            continue
        if op == "shutdown":
            reply({"ok": True})
            return 0
        handler = OPS.get(op)
        if handler is None:
            reply(err(f"unknown op {op!r}"))
            continue
        try:
            reply(handler(req))
        except Exception as e:  # noqa: BLE001 - one bad op must not kill the sidecar
            reply(err(f"{op}: {type(e).__name__}: {e}"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
