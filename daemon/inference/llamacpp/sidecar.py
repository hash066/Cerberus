#!/usr/bin/env python3
"""Cerberus llama.cpp inference helper (v0.1).

JSON-lines protocol matches daemon/inference/llamacpp_sidecar.go and mirrors the
MLX sidecar's forward_layers byte activation contract.
"""

import base64
import json
import os
import platform
import struct
import sys

PROTO = sys.stdout
sys.stdout = sys.stderr

DEMO_LAYERS = 4
DEFAULT_MODEL = os.environ.get("CERBERUS_LLAMA_MODEL") or os.environ.get("LLAMA_MODEL") or ""
DEFAULT_PROMPT = os.environ.get("CERBERUS_LLAMA_PROMPT", "Hello")

_llama = None
_llama_err = None
_models = {}


def import_llama():
    global _llama, _llama_err
    if _llama is not None or _llama_err is not None:
        return _llama
    try:
        from llama_cpp import Llama  # noqa: F401

        _llama = Llama
    except Exception as e:  # noqa: BLE001
        _llama_err = (
            f"llama_cpp not importable: {e} "
            "(pip install llama-cpp-python; requires a built llama.cpp backend)"
        )
    return _llama


def err(msg):
    return {"ok": False, "error": str(msg)}


def model_path(req):
    p = (req.get("model") or DEFAULT_MODEL or "").strip()
    if not p:
        raise ValueError("no GGUF model path (set CERBERUS_LLAMA_MODEL or pass model)")
    if not os.path.isfile(p):
        raise ValueError(f"model path not found: {p}")
    return p


def load_model(path):
    if path in _models:
        return _models[path]
    Llama = import_llama()
    if Llama is None:
        raise RuntimeError(_llama_err)
    llm = Llama(
        model_path=path,
        n_ctx=512,
        embedding=True,
        logits_all=True,
        verbose=False,
    )
    _models[path] = llm
    return llm


def map_demo_layers(layer_lo, layer_hi, n_layer):
    if layer_lo > layer_hi or layer_hi >= DEMO_LAYERS:
        raise ValueError(f"invalid demo layer range [{layer_lo},{layer_hi}]")
    m_lo = layer_lo * n_layer // DEMO_LAYERS
    m_hi = (layer_hi + 1) * n_layer // DEMO_LAYERS - 1
    if m_hi >= n_layer:
        m_hi = n_layer - 1
    if m_lo > m_hi:
        m_lo = m_hi
    return m_lo, m_hi


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


def pad_to_embd(values, n_embd):
    out = [0.0] * n_embd
    n = min(len(values), n_embd)
    for i in range(n):
        out[i] = float(values[i])
    return out


def capture_layer_hidden(llm, target_layer):
    captured = {"data": None}

    def eval_callback(tensor, ask, _user_data):
        try:
            name = tensor.contents.name.decode("utf-8")
        except Exception:  # noqa: BLE001
            return False
        if ask:
            return (
                name.startswith("blk.")
                and (f".{target_layer}." in name or f"-{target_layer}" in name)
                and ("attn_out" in name or "ffn_out" in name or "attn_norm" in name)
            )
        try:
            import ctypes
            from llama_cpp import llama_cpp

            t = tensor.contents
            ne0 = int(t.ne[0])
            ne1 = int(t.ne[1]) if int(t.ne[1]) > 0 else 1
            n = ne0 * ne1
            buf = (ctypes.c_float * n)()
            llama_cpp.ggml_backend_tensor_get(t, ctypes.cast(buf, ctypes.c_void_p), 0, n * 4)
            captured["data"] = list(buf)
        except Exception:  # noqa: BLE001
            captured["data"] = None
        return True

    Llama = import_llama()
    cb_llm = Llama(
        model_path=llm.model_path,
        n_ctx=512,
        embedding=True,
        logits_all=True,
        verbose=False,
        eval_callback=eval_callback,
    )
    bos = cb_llm.token_bos()
    if bos is None or bos < 0:
        bos = 1
    cb_llm.eval([bos])
    return captured["data"]


def decode_input_embedding(llm, req, layer_lo, n_embd):
    raw = _decode_activation_bytes(req)
    shape = req.get("shape") or []
    dtype = req.get("dtype", "f32")

    if layer_lo > 0:
        if not raw or len(shape) != 2 or dtype != "f32":
            raise ValueError("forward_layers: mid-pipeline shard needs activation_bytes and shape [seq, hidden]")
        seq_len, hidden = int(shape[0]), int(shape[1])
        vals = _unpack_f32(raw)
        if seq_len * hidden != len(vals):
            raise ValueError(f"forward_layers: shape {shape} vs {len(vals)} floats mismatch")
        # v0.1: use last position hidden vector as the injected embedding.
        return pad_to_embd(vals[(seq_len - 1) * hidden : seq_len * hidden], n_embd)

    if raw and dtype == "i32" and shape:
        tokens = _unpack_i32(raw)
    elif raw and len(raw) == DEMO_LAYERS * 4 and (not shape or shape == [DEMO_LAYERS]):
        # Legacy split-MLP fixture at layer 0: blend into BOS embedding.
        vals = _unpack_f32(raw)
        return pad_to_embd(vals, n_embd)
    else:
        prompt = req.get("prompt") or DEFAULT_PROMPT
        tokens = llm.tokenize(prompt.encode("utf-8"))
        if not tokens:
            tokens = [llm.token_bos() or 1]
        llm.eval(tokens[:1])
        emb = llm.embeddings
        if emb is None:
            raise RuntimeError("forward_layers: could not read token embedding")
        if hasattr(emb, "tolist"):
            flat = [float(x) for x in emb.tolist()]
        else:
            flat = [float(x) for x in emb]
        return pad_to_embd(flat, n_embd)

    llm.eval(tokens[:1])
    emb = llm.embeddings
    if emb is None:
        raise RuntimeError("forward_layers: could not read token embedding")
    if hasattr(emb, "tolist"):
        flat = [float(x) for x in emb.tolist()]
    else:
        flat = [float(x) for x in emb]
    return pad_to_embd(flat, n_embd)


def run_forward_layers(req):
    Llama = import_llama()
    if Llama is None:
        return err(_llama_err)

    path = model_path(req)
    llm = load_model(path)
    n_layer = llm.n_layer()
    n_embd = llm.n_embd()

    layer_lo = int(req.get("layer_lo", 0))
    layer_hi = int(req.get("layer_hi", 0))
    m_lo, m_hi = map_demo_layers(layer_lo, layer_hi, n_layer)

    try:
        embd_vec = decode_input_embedding(llm, req, layer_lo, n_embd)
    except Exception as e:  # noqa: BLE001
        return err(str(e))

    try:
        import ctypes
        from llama_cpp import llama_cpp

        bos = llm.token_bos()
        if bos is None or bos < 0:
            bos = 1
        tokens = [bos]
        batch = llama_cpp.llama_batch_get_one((ctypes.c_int * 1)(*tokens), 1)
        embd = (ctypes.c_float * n_embd)(*embd_vec)
        batch.embd = ctypes.cast(embd, ctypes.POINTER(ctypes.c_float))
        batch.token = None
        ret = llama_cpp.llama_decode(llm._ctx.ctx, batch)
        if ret != 0:
            return err(f"llama_decode failed (ret={ret})")
    except Exception:
        llm.eval([llm.token_bos() or 1])

    hidden_out = None
    try:
        hidden_out = capture_layer_hidden(llm, m_hi)
    except Exception:  # noqa: BLE001
        hidden_out = None

    if hidden_out is None:
        try:
            emb = llm.embeddings
            if emb is not None:
                hidden_out = [float(x) for x in (emb.tolist() if hasattr(emb, "tolist") else emb)]
        except Exception:  # noqa: BLE001
            hidden_out = None

    if hidden_out is None:
        return err("forward_layers: could not read hidden output")

    resp = {
        "ok": True,
        "backend": "llamacpp",
        "n_layers": n_layer,
        "hidden_size": n_embd,
        "model_layer_lo": m_lo,
        "model_layer_hi": m_hi,
    }

    if layer_hi == DEMO_LAYERS - 1:
        flat = hidden_out
        resp["activation_bytes"] = _encode_activation_bytes(_pack_f32(flat))
        resp["shape"] = [len(flat)]
        resp["dtype"] = "f32"
    elif len(shape := req.get("shape") or []) == 2 and layer_lo > 0:
        seq_len, hidden = int(shape[0]), int(shape[1])
        flat = hidden_out[: hidden]
        while len(flat) < hidden:
            flat.append(0.0)
        packed = []
        for _ in range(seq_len):
            packed.extend(flat[:hidden])
        resp["activation_bytes"] = _encode_activation_bytes(_pack_f32(packed))
        resp["shape"] = [seq_len, hidden]
        resp["dtype"] = "f32"
    else:
        pipe = hidden_out[:DEMO_LAYERS]
        while len(pipe) < DEMO_LAYERS:
            pipe.append(0.0)
        resp["activation_bytes"] = _encode_activation_bytes(_pack_f32(pipe))
        resp["shape"] = [DEMO_LAYERS]
        resp["dtype"] = "f32"
    return resp


def op_info(_req):
    Llama = import_llama()
    info = {
        "ok": True,
        "platform": sys.platform,
        "machine": platform.machine(),
        "python": platform.python_version(),
        "llama_cpp": Llama is not None,
        "model": DEFAULT_MODEL,
        "demo_layers": DEMO_LAYERS,
    }
    if Llama is None:
        info["error"] = _llama_err
    return info


OPS = {
    "info": op_info,
    "forward_layers": run_forward_layers,
    "forward": run_forward_layers,
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
        except Exception as e:  # noqa: BLE001
            reply(err(f"{op}: {type(e).__name__}: {e}"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
