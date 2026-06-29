# Vertical 10 — Desktop App Model (Daemon · CLI · Tray)

> **Workstream C — Economy, Lifecycle & Surface.** Depends on: 00/03/06 (core via IPC), 08 (telemetry for tray). Stub until integration: stub core daemon. See [docs/workstreams.md](../workstreams.md).

> Form-factor vertical. **Browser architectures are strictly forbidden.** Cerberus is a local desktop application: a headless daemon, a CLI, and a native System Tray UI. Conforms to [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Purpose & Responsibilities
- Deliver Cerberus as three local surfaces: **`cerberusd`** (headless daemon), **`cerberus`** (CLI), **Tray UI** (Tauri v2, tray-first).
- Provide the local **IPC contract** between them, the install-time **operator capability** bootstrap, and an **OpenAI-compatible gateway** for drop-in agent workloads.
- Run the **OTP-style supervision tree** that owns all other verticals' subsystems.

## 2. Position in the System
- **Surface + control plane host.** The daemon hosts verticals 00–09; the CLI/tray are thin capability-bearing clients.

## 3. Detailed Architecture
```
   ┌──────────────┐   ┌──────────────┐         ┌──────────────────────────────┐
   │ cerberus CLI │   │  Tray UI     │         │  external agent / SDK         │
   │ (Go)         │   │ (Tauri v2)   │         │  openai.base_url=localhost     │
   └──────┬───────┘   └──────┬───────┘         └───────────────┬──────────────┘
          │ gRPC over UDS / named pipe (cap-gated)             │ HTTP (localhost only)
          └─────────────────┬─────────────────────────────────┘
                            ▼
                 ┌──────────────────────────────┐
                 │       cerberusd (Go)          │
                 │  supervision tree → 00..09    │
                 │  gateway: OpenAI-compatible    │
                 └──────────────────────────────┘
```
- **No browser, ever.** The dashboard is a **Tauri v2** app driving the OS-native webview (WKWebView/WebView2/WebKitGTK) from a Rust core — the entire UI is a small native binary, not an embedded Chromium. The tray is the primary surface (topology graph, memory/VRAM rings, thermal indicators, mesh status); a window opens on demand. (Volume I's "Electron trap" is explicitly avoided.)
- **Daemon (`cerberusd`):** the long-lived process; owns the supervision tree ([01](01-mesh-fabric-transport.md)) and every vertical subsystem; survives CLI/tray restarts.
- **CLI (`cerberus`):** `up`, `status`, `peers`, `mount`, `run <component>`, `cap mint|attenuate|revoke`, `profile`. Scriptable; holds an attenuated operator capability.
- **IPC:** gRPC over Unix domain socket (`$XDG_RUNTIME_DIR/cerberus.sock`) / Windows named pipe. **No TCP admin surface.** Every RPC carries a capability ([schemas §3.8](../schemas/schemas.md)).
- **Gateway:** an OpenAI-compatible HTTP endpoint on `localhost` (Volume I §5) so existing agent frameworks point at Cerberus unchanged; requests are placed by the scheduler (06) and executed across the mesh (03).

## 4. Data Structures / Wire Formats
- IPC: gRPC service `Cerberus` with cap-bearing metadata ([schemas §3.8](../schemas/schemas.md)).
- Operator capability: minted at install, stored in OS keychain / Secure Enclave; root of the local capability tree ([07](07-identity-cap-lifecycle.md)).
- Tray↔daemon: subscribes to telemetry/trace streams (08) over the same IPC.

## 5. Interfaces / APIs
```protobuf
service Cerberus {
  rpc Up(UpReq) returns (UpResp);
  rpc Status(StatusReq) returns (stream NodeTelemetry);   // tray live view
  rpc Mount(MountReq) returns (MountResp);                // 9P mount (04)
  rpc Run(RunReq) returns (stream RunEvent);              // dispatch component (03)
  rpc Cap(CapReq) returns (CapResp);                      // mint/attenuate/revoke (00/07)
  rpc SetProfile(ProfileReq) returns (ProfileResp);       // open_mesh | sealed
}
```

## 6. Tech Stack
| Concern | Choice | Why |
|---|---|---|
| Daemon / CLI | Go 1.22+ | concurrency, cross-compile, hosts the control plane |
| Tray / UI | **Tauri v2** (tray-first), native webview | <15 MB, ~zero idle RAM; **no Chromium/browser** |
| IPC | gRPC over UDS / named pipe | local-only, typed, streaming |
| Key custody | OS keychain / Secure Enclave | protect the operator capability |
| Gateway | Go HTTP, OpenAI-compatible | drop-in for existing agent SDKs |

## 7. Security Model
- **No remote admin by default:** IPC is local-only (UDS/pipe), capability-gated; the gateway binds `localhost`.
- The operator capability is attenuable/revocable ([07](07-identity-cap-lifecycle.md)) — no unrevocable god key.
- Tray/CLI cannot exceed the rights of the capabilities they hold; even "admin" actions are capability invocations.

## 8. Open Mesh vs Sealed
- **OpenMesh:** tray surfaces wallet/economy panels ([05](05-eutxo-open-mesh-economy.md)); permissive local gateway.
- **Sealed:** economy panels hidden; tray surfaces attestation/audit status; gateway may require a stronger local capability.

## 9. Failure Modes & Mitigations
| Failure | Mitigation |
|---|---|
| Daemon crash | OS service manager restarts; supervision tree rebuilds subsystems; CRDT state recovers |
| Tray/CLI crash | stateless clients reconnect to daemon; no mesh impact |
| IPC socket hijack | filesystem perms on UDS + capability check on every RPC |
| User expects a web UI | by design: native tray only; remote access is a deliberate non-feature |

## 10. Verdict
**Shippable.** Tauri v2 + Go daemon + gRPC-over-UDS is a proven desktop pattern; the OpenAI-compatible gateway is straightforward. Open question: whether to offer an optional, capability-gated, LAN-scoped read-only status view for headless servers (without violating the no-browser-admin principle).
