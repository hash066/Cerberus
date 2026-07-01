# `cerberus` — CLI Reference

The `cerberus` command is the operator's control client. It talks to a running
`cerberusd` over the local RPC socket (`127.0.0.1:9092`) and authenticates every
call with a capability token. It is intentionally small in v0.1 — the control
surface, not a batteries-included toolbox.

- Start the daemon first: see [docs/getting-started.md](getting-started.md).
- For programmatic compute, use the [gateway](gateway.md), not the CLI.

---

## Synopsis

```
cerberus <command>
```

Run without arguments to see usage:

```bash
$ cerberus
Usage: cerberus <command>
Commands: status, run
Auth: reads $CERBERUS_TOKEN or the operator token file written by cerberusd.
```

Build/run the CLI:

```bash
go run ./cmd/cerberus <command>
# or build once:
go build -o cerberus ./cmd/cerberus
./cerberus <command>
```

---

## Authentication

Every command presents a capability token to the daemon. The CLI resolves it in
this order:

1. **`$CERBERUS_TOKEN`** — if set, used verbatim.
2. **The operator token file** written by `cerberusd` on startup, in your OS
   config dir (`%AppData%\cerberus\operator.token` on Windows;
   `~/Library/Application Support/cerberus/operator.token` on macOS;
   `~/.config/cerberus/operator.token` on Linux).

If neither is present:

```
no capability token found: set $CERBERUS_TOKEN or start cerberusd (writes <path>)
```

Start `cerberusd` (which writes the token) or export `CERBERUS_TOKEN`. See
[getting-started §4](getting-started.md#4-the-operator-token) for how to load the
token into your shell.

The operator token carries `admin` rights, which satisfy the `read` right the
`status` command requires. You can point the CLI at a **narrower** token via
`$CERBERUS_TOKEN` if you want to test least-privilege behaviour — a token without
`read`/`admin` will be rejected with `unauthorized`.

---

## Commands

### `status`

Query the daemon's live state. Requires a token granting `read` (or `admin`).

```bash
$ cerberus status
Cerberus Daemon Status
Version: 0.1.0
State:   Running (Power: AC, Battery: 100.0%)
Auth:    authenticated as "operator"
```

| Field | Meaning |
|---|---|
| **Version** | the frozen contract version the daemon reports |
| **State** | lifecycle state, including power source and battery percentage from the lifecycle monitor |
| **Auth** | the subject of the verified token (proves your token authenticated) |

This is the CLI counterpart of the richer JSON on the status API
(`GET http://127.0.0.1:7777/api/v1/status`), which also exposes profile, kernel,
uptime, mesh/peers, and the operator credit balance — see
[getting-started §7](getting-started.md#7-view-metrics-and-health).

**Errors**

- `unauthorized: <reason>` — the token is missing the `read` right, expired, or
  was revoked.
- `Failed to connect to cerberusd: ...` — the daemon isn't listening on
  `127.0.0.1:9092`; start it first.

---

### `run`

```bash
$ cerberus run
Run command stub. Will dispatch to gateway/scheduler in the future.
```

> **Status: stub.** In v0.1 `run` prints a placeholder and does **not** dispatch a
> workload. Do not script against it yet. This is called out honestly rather than
> faked.
>
> **What to use instead:** the real, working way to run compute is the
> **OpenAI-compatible gateway** on `:8080`, which dispatches a `ComputeTask`
> through the real WASM executor. See [docs/gateway.md](gateway.md). The
> acceptance demo (`go run ./test/e2e`) shows the underlying remote-execution path
> end-to-end (returns `1337`).

When `run` is implemented it will submit a workload to the scheduler/gateway from
the CLI; until then it is a labelled extension point.

---

### Unknown commands

Any other argument prints an error and exits non-zero:

```bash
$ cerberus frobnicate
Unknown command: frobnicate
```

---

## Exit codes

| Code | When |
|---|---|
| `0` | success |
| `1` | usage error, missing token, RPC/connection failure, unauthorized, or unknown command |

---

## Notes & roadmap

- The CLI talks plain Go `net/rpc` over TCP `127.0.0.1:9092` (chosen over a Unix
  socket for cross-platform simplicity in the v0.1 skeleton). The socket is **not**
  an open backdoor: the daemon authorizes every RPC method against the presented
  token.
- Planned surface (not yet implemented): `run` (dispatch a workload), peer
  listing, capability mint/attenuate/revoke, and profile introspection. Today,
  peer/credit/power detail is available via the status API JSON and the
  `/metrics` endpoint.

See also: [docs/gateway.md](gateway.md) · [docs/user-guide.md](user-guide.md) ·
[docs/getting-started.md](getting-started.md).
