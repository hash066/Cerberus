# `cerberus-mcp` — MCP server for AI coding tools

> Lets Claude Code, Cursor, or any other MCP (Model Context Protocol) client
> operate a running Cerberus daemon directly — the same way those tools talk to
> their own built-in MCP servers — instead of you hand-writing HTTP calls or
> shelling out to the `cerberus` CLI on the model's behalf.

- New to Cerberus? Start with [docs/getting-started.md](getting-started.md) to
  get a daemon running first — `cerberus-mcp` has nothing to talk to without one.
- Want the human CLI instead? See [docs/cli.md](cli.md).

---

## What this is

`cmd/cerberus-mcp` is a small Go binary that speaks MCP (JSON-RPC 2.0) over
stdio — the transport Claude Code and Cursor both use for locally-installed
MCP servers — and exposes a running `cerberusd`'s control-plane RPC as a set of
MCP tools. It is a translation layer, not a new control path: every tool wraps
the exact same `DaemonRPC` methods the `cerberus` CLI calls
(`cmd/cerberusd/rpc.go`), reusing their request/response shapes, and the same
`/metrics` HTTP endpoint the CLI's `metrics` command fetches.

It is built on the official Go MCP SDK,
[`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk)
(`mcp` package) — no hand-rolled JSON-RPC framing.

### Tools exposed

| Tool | Wraps | Notes |
|---|---|---|
| `cerberus_status` | `DaemonRPC.Status` | daemon health, identity, mesh, balance |
| `cerberus_run_workload` | `DaemonRPC.Run` | **flagship tool** — executes a WASM component (base64-encoded in the call) locally or on a named mesh peer, returns the real result |
| `cerberus_list_nodes` | `DaemonRPC.Nodes` | mesh peers (self + connected) |
| `cerberus_list_devices` | `DaemonRPC.Devices` | 9P namespace devices |
| `cerberus_wallet_balance` | `DaemonRPC.Wallet` | compute-credit balance from the durable eUTXO ledger |
| `cerberus_caps_mint` | `DaemonRPC.CapsMint` | mint a capability token (requires admin) |
| `cerberus_caps_attenuate` | `DaemonRPC.CapsAttenuate` | derive a narrower token from a parent (delegation) |
| `cerberus_caps_revoke` | `DaemonRPC.CapsRevoke` | revoke a token by id (durable + mesh-gossiped) |
| `cerberus_caps_list` | `DaemonRPC.CapsList` | tokens minted by this daemon + revocation status |
| `cerberus_conflicts_list` | `DaemonRPC.ConflictsList` | open CRDT belief conflicts for a doc |
| `cerberus_conflicts_resolve` | `DaemonRPC.ConflictsResolve` | record a resolution for a conflicted subject |
| `cerberus_metrics` | `GET /metrics` | Prometheus metrics, both as parsed structured samples (`name`/`labels`/`value`) and the raw exposition text |

Every tool's input/output schema is generated automatically by the SDK from Go
struct tags (`mcp.AddTool`), so a client's `tools/list` call always reflects
the real Go types — see `cmd/cerberus-mcp/tools.go`.

---

## Building it

```bash
go build -o bin/cerberus-mcp ./cmd/cerberus-mcp
```

Deliberately **no `.exe` suffix** in the `-o` path, even on Windows: `go build`
still produces a native PE executable there (just without the extension), and
that's what makes a single checked-in `command` path in `.mcp.json`/
`.cursor/mcp.json` work unmodified on Windows, macOS, and Linux alike — Win32's
`CreateProcess` (which is what both Claude Code and Cursor use to spawn an MCP
server) runs an extensionless executable directly given a full path; only
`PATH`-search resolution needs the `.exe` suffix, and neither client does that
kind of lookup for a `command` value containing a path separator.

`bin/` is gitignored (it's a build artifact), so the binary is not checked in —
build it once locally after cloning.

---

## Pointing Claude Code / Cursor at it

Both are pre-wired via checked-in config — **you don't need to add anything**
once the binary is built:

- **Claude Code**: reads project-scoped MCP servers from [`.mcp.json`](../.mcp.json)
  at the repo root. It points `command` at
  `${CLAUDE_PROJECT_DIR}/bin/cerberus-mcp` (`${CLAUDE_PROJECT_DIR}` is Claude
  Code's stable project-root variable, expanded at connect time — see
  [Claude Code's MCP docs](https://code.claude.com/docs/en/mcp) for the
  expansion syntax). The first time you open this repo in Claude Code it will
  prompt you to approve the project-scoped server (`.mcp.json` servers are
  untrusted by default until you accept the workspace).
- **Cursor**: reads project-scoped servers from
  [`.cursor/mcp.json`](../.cursor/mcp.json), same `mcpServers` shape. It points
  `command` at the relative path `./bin/cerberus-mcp`, which Cursor resolves
  against the project root it spawns the server from. The leading `./` is
  deliberate: a bare `bin/cerberus-mcp` (no path separator prefix) is
  ambiguous to spawn on Windows — some spawn implementations treat a relative
  path without a leading `./`/`../` as a `PATH`-search rather than a
  cwd-relative path, exactly the class of bug reported against Cursor's own
  MCP config resolution on Windows.

If you use a different MCP-compatible tool, point it at the built binary the
same way (no arguments, no special flags — it only speaks MCP-over-stdio) and
give it whatever token/discovery environment it needs (see below).

---

## Daemon discovery (finding a running `cerberusd`)

`cerberus-mcp` resolves the daemon's real bind addresses in the same order the
`cerberus` CLI and the tray converge on, via `daemon/discovery`:

1. **`$CERBERUS_RPC_ADDR` / `$CERBERUS_METRICS_ADDR`** — explicit override, if
   either is set.
2. **The discovery manifest** (`daemon/discovery.Read()`), written by a running
   `cerberusd` once its subsystems finish binding — the *actual* addresses it
   bound to, not a guess. Lives at `daemon/discovery.Path()` (e.g.
   `%AppData%\cerberus\daemon.json` on Windows).
3. **Hardcoded defaults** — `127.0.0.1:9092` (RPC), `127.0.0.1:7779` (metrics) —
   the same fallback `cmd/cerberus/main.go` uses, so behavior matches the CLI
   when no manifest is present (e.g. an older `cerberusd` build).

`cerberus-mcp` logs which source it resolved to on startup, to stderr (stdout
is the JSON-RPC channel and must stay clean).

---

## Auth (no ambient authority)

Every tool call presents the operator capability token — `cerberus-mcp` has no
authority of its own beyond what that token grants (CLAUDE.md's ocap rule).
Token resolution:

1. At process startup, `daemon/auth.LoadToken()` — `$CERBERUS_TOKEN`, else the
   operator token file `cerberusd` writes (`daemon/auth.OperatorTokenPath()`).
   This becomes the default token every tool call uses.
2. **Per-call override**: every tool accepts an optional `token` argument, so a
   caller (or a test) can present a narrower/attenuated capability instead of
   the process-wide default — useful for exercising least-privilege behavior
   without restarting the server.

If neither is available for a given call, the tool returns a normal MCP tool
error (`isError: true`) explaining how to fix it — it does not silently
proceed with no token, and it never embeds or hardcodes one.

---

## Testing

MCP is JSON-RPC over stdio, so the tests in `cmd/cerberus-mcp/tools_test.go`
don't spawn a real process or pipe real OS stdio. They stand up a real
`mcp.Server` (with the same `registerTools` production code) and a real
`mcp.Client`, connect them over the SDK's `mcp.NewInMemoryTransports()`, and
drive `tools/list` / `tools/call` exactly as a real client would — against a
scriptable fake daemon backend (`fake_backend_test.go`) instead of a live
`cerberusd`. This covers the full protocol path (schema generation,
JSON-RPC framing, structured output, tool-error semantics) while staying a
fast, hermetic `go test`.

Run just this package:

```bash
go test ./cmd/cerberus-mcp/...
```

---

## Maturity notes

- `cerberus_run_workload`'s remote path (`on: "<peer-hex>"`) depends on the
  mesh having composed and a worker actually serving compute — if not, the
  underlying `DaemonRPC.Run` call returns a clear error rather than a faked
  success, and that error surfaces through this tool unchanged (CLAUDE.md's
  maturity-honesty rule).
- `cerberus_metrics`'s structured-sample parser is a minimal Prometheus text
  exposition reader (gauges/counters as flat samples); it does not reconstruct
  histogram/summary buckets.
