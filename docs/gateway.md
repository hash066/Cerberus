# The OpenAI-Compatible Gateway

Cerberus embeds an **OpenAI-compatible HTTP gateway** so existing agent SDKs and
tools (the `openai` Python/JS clients, LangChain, AutoGPT, curl, etc.) can drive
your local mesh by changing one line: the `base_url`. Unlike hosted APIs, the
gateway enforces Cerberus's **zero-ambient-authority** posture — every request
must present a capability token, and there is no unauthenticated path.

- Endpoint: **`http://localhost:8080`** (localhost-bound; not a remote admin surface).
- Auth: **`Authorization: Bearer <token>`**, token must grant the `exec` right.
- Start the daemon first: [docs/getting-started.md](getting-started.md).

---

## What the gateway does (and doesn't) today

The gateway is a **real, production-shaped path**: it authenticates the caller,
validates and bounds the request, maps it to a Cerberus `ComputeTask`, and
dispatches it through the **real WebAssembly executor** (wazero), returning an
OpenAI-shaped envelope. The scheduler/runtime place and run the task on the mesh.

> [!WARNING]
> **Without a chat backend, this gateway answers for models it does not have,
> and its token counts are fabricated.** Both are known bugs; read this before
> you build against it.
>
> `componentFor` ([`gateway.go:160`](../daemon/gateway/gateway.go)) resolves an
> unknown model name straight to a component CID, which falls through to the
> seeded `hello-shard`. So on a stock daemon, `POST {"model":"gpt-4o"}` returns
> **HTTP 200** with `"content": "1337"` — verified by running it. An unknown
> model should 404.
>
> Worse, that path's `usage` block is invented: `prompt_tokens` is the
> **character count** of your prompt and `completion_tokens` is the **byte
> length** of the reply ([`chat.go:186`](../daemon/gateway/chat.go)). There is no
> tokenizer in it. A field named `prompt_tokens` holding a character count is a
> fabricated measurement, and every OpenAI client will read it as tokens.
>
> **Run `cerberusd -llama-model <file.gguf>` and this becomes a different
> endpoint** — a real `llama-server` serves it, with a real tokenizer, sampler
> and genuine `usage`. The bug is the fallback, not the llama path.

**Be aware, honestly:**
- **Two modes.** With `-llama-model`, a real llama.cpp model is served
  (`cmd/cerberusd/main.go:509` → `gw.SetInference`). Without it, the wired
  component is the demo `hello-shard` WASM module and the `content` you get back
  is that component's output — genuine OpenAI *shape*, WASM shard behind it.
- **`/v1/models` lists what is registered — WASM shards, plus your llama model
  if you passed one.** `BuiltinInferenceModels()` returns nil by design; a stock
  daemon logs `0 inference model(s) registered on gateway` at boot. The split-MLP
  fixture is deliberately **not** advertised here — a fixture in a test is
  honest; a fixture on `/v1/models` pretending to be a chat model is not.
- **Distributed inference is not wired.** `-llama-rpc` exists but the forwarder
  that would bridge a local `llama-server` to a capability-gated remote worker is
  constructed by no binary. Single-node chat works; splitting a model across
  peers does not. See [README.md](../README.md).

---

## Endpoints

| Method | Path | Status | Auth (right) |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | ✅ implemented — real LLM with `-llama-model`; **otherwise answers to any model name, see the warning above** | `exec` |
| `GET`  | `/v1/models` | ✅ implemented (WASM shards + your `-llama-model`, if any) | `exec`/`read` |
| —      | streaming (`stream: true`) | ✅ implemented — real SSE: role-priming chunk, content deltas, `finish_reason`, `data: [DONE]` | `exec` |

---

## `POST /v1/chat/completions`

### Request

The gateway accepts the common subset of the OpenAI chat-completions body. It
decodes **strictly**: unknown JSON fields and trailing data are rejected.

```jsonc
{
  "model": "cerberus-shard",          // required, non-empty
  "messages": [                        // required, 1..256 messages
    { "role": "user", "content": "hello" }
  ]
}
```

Validation limits (enforced before any compute is spent):

| Rule | Limit |
|---|---|
| Request body size | ≤ 1 MiB |
| Number of messages | 1 … 256 |
| Each message | non-empty `role` and non-empty `content` |
| Total content across messages | ≤ 512 KiB |

Violations return an OpenAI-shaped error (see [Errors](#errors)).

### Response

```json
{
  "id": "chatcmpl-cerberus",
  "object": "chat.completion",
  "created": 1751000000,
  "model": "cerberus-shard",
  "choices": [
    {
      "index": 0,
      "message": { "role": "assistant", "content": "..." }
    }
  ]
}
```

- `model` echoes your request.
- `content` is the output of the dispatched component (the demo shard in v0.1).
- The task is scoped to the **subject of your verified token**, never to
  client-supplied identity.

---

## curl examples

Assuming `$CERBERUS_TOKEN` holds your operator token
([how to load it](getting-started.md#4-the-operator-token)):

**Basic chat completion**

```bash
curl -sS http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $CERBERUS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
        "model": "cerberus-shard",
        "messages": [{"role": "user", "content": "hello"}]
      }'
```

**Missing token → 401**

```bash
curl -i http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"cerberus-shard","messages":[{"role":"user","content":"hi"}]}'
# HTTP/1.1 401 Unauthorized
# {"error":{"message":"missing bearer token","type":"invalid_request_error"}}
```

**Malformed request → 400**

```bash
curl -sS http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $CERBERUS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"cerberus-shard"}'   # no messages
# {"error":{"message":"messages must contain at least one message","type":"invalid_request_error"}}
```

---

## Python (`openai` client) example

Because the endpoint is OpenAI-compatible, the official `openai` client works —
you only change `base_url` and use your Cerberus token as the `api_key`.

```python
import os
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",   # the Cerberus gateway
    api_key=os.environ["CERBERUS_TOKEN"],  # your operator (or exec-scoped) token
)

resp = client.chat.completions.create(
    model="cerberus-shard",
    messages=[{"role": "user", "content": "hello"}],
)

print(resp.choices[0].message.content)
```

The `api_key` is sent by the client as `Authorization: Bearer <token>` — exactly
what the gateway checks for the `exec` right. Point any OpenAI-compatible tool
(LangChain, LlamaIndex, AutoGPT, etc.) at the same `base_url` + token.

> **Note.** With the demo shard wired in v0.1, `content` is the shard's output, not
> generated text. When a real LLM component is wired behind the gateway, this exact
> client code returns model text unchanged.

---

## Streaming (planned)

Token streaming (`stream: true` → `text/event-stream` with `chat.completion.chunk`
deltas and a terminal `data: [DONE]`) is **not implemented in v0.1**. The intended
client shape, once available, is the standard:

```python
stream = client.chat.completions.create(
    model="cerberus-shard",
    messages=[{"role": "user", "content": "hello"}],
    stream=True,
)
for chunk in stream:
    delta = chunk.choices[0].delta.content or ""
    print(delta, end="", flush=True)
```

`stream: true` is real. The gateway emits a role-priming chunk, one or more
content deltas, a `finish_reason` chunk, then `data: [DONE]`, flushing as it
writes. Note that strict decoding rejects unknown fields — send only the fields
documented here.

---

## `GET /v1/models`

Implemented. Returns the OpenAI `{"object":"list","data":[...]}` shape describing
the components the gateway can dispatch:

```json
{"object":"list","data":[{"id":"hello-shard","object":"model","created":1784174905,
 "owned_by":"cerberus","component_cid":"hello-shard"}]}
```

**These are WASM components, not language models**, and the list is exactly as
short as it looks — a stock daemon advertises `hello-shard` and nothing else.

Do **not** infer that a name absent from this list is rejected by
`/v1/chat/completions`: it isn't (see the warning at the top). `model` is
currently a free-form label that gets echoed back, resolved to a component CID,
and used to scope the task id.

---

## Errors

Errors use OpenAI's `{"error": {...}}` envelope so SDKs surface them cleanly:

```json
{ "error": { "message": "unauthorized: token expired", "type": "invalid_request_error" } }
```

| HTTP | `type` | Cause |
|---|---|---|
| `400` | `invalid_request_error` | invalid JSON, unknown/trailing fields, missing `model`/`messages`/`role`/`content` |
| `401` | `invalid_request_error` | missing Bearer token, or token lacks `exec` / expired / revoked |
| `405` | `invalid_request_error` | wrong method (only `POST` is allowed on chat completions) |
| `413` | `invalid_request_error` | body > 1 MiB, > 256 messages, or > 512 KiB total content |
| `500` | `server_error` | dispatch/execution failed inside the daemon |

---

## Security notes

- **Localhost only.** The gateway binds `:8080` on the local host and is not a
  remote admin surface (ARCHITECTURE §1.1, vertical 10). To reach it from another
  machine, front it with your own authenticated tunnel/proxy — don't expose it raw.
- **Least privilege.** The operator token has `admin` (which includes `exec`). For
  a specific tool or agent, mint/attenuate a token scoped to just `exec` (and
  optionally a resource path) and hand *that* out. See the capability model in
  [docs/user-guide.md](user-guide.md#capabilities-in-plain-terms).
- **Server-set timeouts** (read-header 10s, read 30s, write 60s, idle 120s)
  prevent a slow client from pinning a connection.

See also: [docs/getting-started.md](getting-started.md) ·
[docs/cli.md](cli.md) · [docs/user-guide.md](user-guide.md).
