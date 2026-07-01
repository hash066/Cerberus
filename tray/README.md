# Cerberus tray — desktop dashboard (Tauri v2)

The native desktop surface for Cerberus (vertical 10). A small Tauri v2 app: a
Rust core (`src-tauri/`) that performs every authenticated network call to the
daemon, and a webview UI (`src/`) that only renders. **No browser, no Chromium**
— the OS-native webview (WebView2 / WKWebView / WebKitGTK) is driven from Rust.

## What it shows & does

Views (sidebar nav): **Overview** (daemon version/profile/kernel/uptime, health,
power/lid), **Mesh peers**, **Devices** (9P namespace), **Workloads**,
**Metrics** (live counters + sparklines), **Wallet** (balance, operator cap),
**Conflicts** (belief-conflicts).

Actions: run a workload (gateway), grant/pool a device, resolve a conflict,
revoke a capability, copy the operator token.

## How the data flows (real, no fakes)

The webview never touches the network. It calls Rust `#[tauri::command]`s in
`src-tauri/src/lib.rs`, which read the **operator capability token** from the OS
config dir (`<config>/cerberus/operator.token`, the same path the Go daemon
writes) and make the authenticated requests:

| Surface | Endpoint | Used for |
|---|---|---|
| status API | `127.0.0.1:7777` `GET /api/v1/status` | Overview / Mesh / Wallet / Power |
| metrics | `127.0.0.1:7779` `GET /metrics` (+ `/healthz`, `/readyz`) | Metrics view, health card |
| gateway | `127.0.0.1:8080` `POST /v1/chat/completions` | "Run a workload" action |

When the daemon is down, the dashboard shows a clear **disconnected** state
(banner + red status dot) and keeps retrying.

### Maturity honesty

Some subsystems are real in the Go daemon but have **no HTTP route yet**: the 9P
device namespace (`daemon/ninep`), the belief-conflict list/resolve
(`daemon/state`), capability revocation over HTTP (`daemon/auth`), and workload
history (scheduler). The Rust commands are wired to the **intended** routes and,
until the daemon serves them, return a typed `PENDING_DAEMON` result the UI
renders as a calm "pending daemon support" note — never fake data, never fake
success. The moment the daemon exposes those routes, the desktop lights up with
no UI change.

## Run the real app (needs the Tauri toolchain + a desktop)

```
# 1. start the daemon (writes the operator token + serves the APIs)
go run ./cmd/cerberusd
# 2. in another shell, launch the tray (needs Node + the Tauri CLI + WebView2)
cd tray && npm install && npm run tauri dev
```

`tray/src-tauri` is excluded from the repo Cargo workspace (it needs the Tauri
toolchain) and is its own workspace root, so it builds standalone:

```
cd tray/src-tauri && cargo build     # compiles the Rust shell
cargo clippy                          # lints it
```

The full bundled app (native window + webview) can only be verified on a real
desktop with the Tauri CLI — that is an accepted human/hardware gate.

## Review the design without a desktop

Open [`preview.html`](preview.html) in any browser. It is a **self-contained
static mock** (inlined CSS, sample data, no Tauri, no daemon) of the full
dashboard, clearly labelled as a mock, so the UI can be reviewed headlessly.
