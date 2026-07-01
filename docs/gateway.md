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

**Be aware, honestly:**
- The **wired component in v0.1 is the demo `hello-shard` WASM module**, not a
  large language model. So the `assistant` message `content` you get back is that
  component's output — the request/response *shape* is genuine OpenAI, the *model*
  behind it is the demo shard. Swapping in a real LLM component is a
  component-swap behind the same interface, not a rewrite.
- **`/v1/chat/completions` (POST)** is implemented.
- **`/v1/models` and streaming (`stream: true`, SSE)** are **not yet implemented**
  in the v0.1 gateway. Sections below show the intended client shape and mark
  clearly what works now versus what is planned.

---

## Endpoints

| Method | Path | Status | Auth (right) |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | ✅ implemented | `exec` |
| `GET`  | `/v1/models` | 🚧 planned (not in v0.1) | `exec`/`read` |
| —      | streaming (`stream: true`) | 🚧 planned (not in v0.1) | `exec` |

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

Until then, requests are handled as a single non-streaming response regardless of
a `stream` field — and note that strict decoding means you should omit fields the
v0.1 gateway doesn't accept (send only `model` and `messages`).

---

## `GET /v1/models` (planned)

A models-list endpoint is **not implemented in v0.1**. When added it will return
the OpenAI `{"object":"list","data":[...]}` shape describing the components the
gateway can dispatch. For now, treat `model` as a free-form label that is echoed
back and used to scope the task id.

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
