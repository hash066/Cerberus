// Cerberus tray app (Tauri v2) — v3 dashboard shell.
//
// The Rust core is the *only* thing that touches the network: it reads the
// operator capability token from the OS config dir (the same path the Go daemon
// writes it to — `auth.OperatorTokenPath`) and performs every authenticated
// request to the daemon's localhost surfaces. The native webview only renders
// the JSON these commands return, so the UI never makes a network call itself —
// consistent with vertical 10's "no browser, ever" boundary.
//
// Daemon surfaces consumed here (all localhost, all Bearer-token-gated except
// the liveness probes):
//   * status API   127.0.0.1:7777  GET /api/v1/status          -> Snapshot JSON
//   * metrics      127.0.0.1:7779  GET /metrics                -> Prometheus text
//                                  GET /healthz  GET /readyz    -> probes (open)
//   * gateway      127.0.0.1:8080  POST /v1/chat/completions   -> run a workload
//
// Honesty note: the daemon does not (yet) expose HTTP endpoints for the device
// (9P) namespace, the belief-conflict list / resolve, capability revocation, or
// device pooling. Those subsystems exist in Go (`daemon/state`, `daemon/ninep`,
// `daemon/auth`) but have no status-API route today, and this lane may only edit
// `tray/`. So the commands for them are wired to the *intended* routes and, when
// those routes 404, return a typed `PENDING_DAEMON` error the UI surfaces as
// "not yet exposed by the daemon" — never fake data, never fake success.

use std::collections::BTreeMap;
use std::path::PathBuf;
use std::sync::Mutex;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tauri::{Manager, RunEvent};
use tauri_plugin_shell::process::CommandChild;
use tauri_plugin_shell::ShellExt;

// ---- daemon endpoints -------------------------------------------------------
//
// These constants are the fallback only. The v3 audit found that cerberusd and
// every client (this tray, the `cerberus` CLI, an MCP server) each hardcoded
// the SAME default ports independently and hoped they'd agree — silently
// wrong on any bind conflict. The Go side now writes a discovery manifest
// (`daemon.json`, next to the operator token) with the addresses it ACTUALLY
// bound to; `daemon_urls()` below reads that manifest first and only falls
// back to these hardcoded constants when it can't (no manifest, unparsable,
// or a field the daemon never bound is empty).

const STATUS_URL: &str = "http://127.0.0.1:7777/api/v1/status";
const METRICS_URL: &str = "http://127.0.0.1:7779/metrics";
const HEALTHZ_URL: &str = "http://127.0.0.1:7779/healthz";
const READYZ_URL: &str = "http://127.0.0.1:7779/readyz";
const GATEWAY_CHAT_URL: &str = "http://127.0.0.1:8080/v1/chat/completions";

// Intended (not-yet-served) routes for actions the daemon has not surfaced over
// HTTP. Kept here so that the day the Go side adds them, the desktop lights up
// with zero UI changes. Until then these 404 and we report PENDING_DAEMON.
//
// These all live under the status API's address, so they participate in the
// same manifest-first resolution as STATUS_URL (see conflicts_url() etc.
// below) rather than staying hardcoded to 127.0.0.1:7777.
const CONFLICTS_PATH: &str = "/api/v1/conflicts";
const RESOLVE_PATH: &str = "/api/v1/conflicts/resolve";
const DEVICES_PATH: &str = "/api/v1/devices";
const WORKLOADS_PATH: &str = "/api/v1/workloads";
const REVOKE_PATH: &str = "/api/v1/cap/revoke";
const GRANT_DEVICE_PATH: &str = "/api/v1/devices/grant";

const HTTP_TIMEOUT: Duration = Duration::from_secs(5);

// ---- discovery manifest ------------------------------------------------------

/// Mirrors the JSON shape `daemon/discovery.Manifest` (Go) writes to
/// `daemon.json`. Field names match the Go struct's `json:"..."` tags exactly
/// -- this is plain JSON, no shared Rust/Go type, by design (the frozen
/// contract is the only cross-language type surface; this manifest is not
/// part of it). Every *_addr field is a bare "host:port" address (NOT a full
/// URL), same as the Go side documents.
///
/// `pid` and `rpc_addr` are deserialized for completeness (they round-trip
/// the full manifest shape and are cheap to keep) but unused today: this
/// binary only ever talks to the daemon's HTTP surfaces (gateway/api/metrics),
/// never the net/rpc control socket, and per the task's guidance we
/// deliberately do NOT add PID-liveness checking in Rust (no cheap
/// cross-platform way to do it without a new dependency) -- a stale manifest
/// just means the addresses we read are wrong, and the existing
/// PENDING_DAEMON / unreachable error handling already reports "daemon down"
/// correctly in that case.
#[derive(Deserialize, Default, Clone)]
#[allow(dead_code)]
struct Manifest {
    #[serde(default)]
    pid: i64,
    #[serde(default)]
    gateway_addr: String,
    #[serde(default)]
    api_addr: String,
    #[serde(default)]
    metrics_addr: String,
    #[serde(default)]
    rpc_addr: String,
}

/// Absolute path of the discovery manifest (OS config dir / cerberus /
/// daemon.json). Mirrors `daemon/discovery.Path()` (Go): same directory as
/// the operator token, so the tray needs only `dirs::config_dir()` to find
/// everything.
fn manifest_path() -> Option<PathBuf> {
    dirs::config_dir().map(|d| d.join("cerberus").join("daemon.json"))
}

/// Best-effort manifest read. Returns None on any failure (file missing,
/// unreadable, malformed JSON) -- the caller treats that exactly like "no
/// manifest" and falls back to the hardcoded constants. We deliberately do
/// NOT attempt PID-liveness checking here (no cheap cross-platform way to do
/// it from Rust without a new dependency): a stale manifest just means the
/// addresses we read are wrong, and the existing PENDING_DAEMON / unreachable
/// error handling in daemon_status/daemon_metrics/etc. already reports
/// "daemon down" correctly when that happens.
fn read_manifest() -> Option<Manifest> {
    let path = manifest_path()?;
    let text = std::fs::read_to_string(path).ok()?;
    serde_json::from_str(&text).ok()
}

/// Build "http://{addr}{path}" from a manifest address field, or None if the
/// field is empty (that subsystem never bound, per the Go side's contract).
fn url_from_addr(addr: &str, path: &str) -> Option<String> {
    if addr.is_empty() {
        None
    } else {
        Some(format!("http://{addr}{path}"))
    }
}

/// Resolve one daemon URL: manifest's address for `path` if the manifest is
/// readable and that field is non-empty, else the hardcoded fallback.
fn resolve_url(pick: impl Fn(&Manifest) -> &str, path: &str, fallback: &str) -> String {
    read_manifest()
        .and_then(|m| url_from_addr(pick(&m), path))
        .unwrap_or_else(|| fallback.to_string())
}

fn status_url() -> String {
    resolve_url(|m| &m.api_addr, "/api/v1/status", STATUS_URL)
}
fn metrics_url() -> String {
    resolve_url(|m| &m.metrics_addr, "/metrics", METRICS_URL)
}
fn healthz_url() -> String {
    resolve_url(|m| &m.metrics_addr, "/healthz", HEALTHZ_URL)
}
fn readyz_url() -> String {
    resolve_url(|m| &m.metrics_addr, "/readyz", READYZ_URL)
}
fn gateway_chat_url() -> String {
    resolve_url(|m| &m.gateway_addr, "/v1/chat/completions", GATEWAY_CHAT_URL)
}
fn conflicts_url() -> String {
    resolve_url(|m| &m.api_addr, CONFLICTS_PATH, &format!("http://127.0.0.1:7777{CONFLICTS_PATH}"))
}
fn resolve_conflict_url() -> String {
    resolve_url(|m| &m.api_addr, RESOLVE_PATH, &format!("http://127.0.0.1:7777{RESOLVE_PATH}"))
}
fn devices_url() -> String {
    resolve_url(|m| &m.api_addr, DEVICES_PATH, &format!("http://127.0.0.1:7777{DEVICES_PATH}"))
}
fn workloads_url() -> String {
    resolve_url(|m| &m.api_addr, WORKLOADS_PATH, &format!("http://127.0.0.1:7777{WORKLOADS_PATH}"))
}
fn revoke_url() -> String {
    resolve_url(|m| &m.api_addr, REVOKE_PATH, &format!("http://127.0.0.1:7777{REVOKE_PATH}"))
}
fn grant_device_url() -> String {
    resolve_url(|m| &m.api_addr, GRANT_DEVICE_PATH, &format!("http://127.0.0.1:7777{GRANT_DEVICE_PATH}"))
}

/// Sentinel prefix a command returns when it hit a daemon route that isn't
/// served yet (HTTP 404) or a feature that has no live endpoint. The webview
/// keys on this to render a calm "pending daemon support" chip instead of an
/// error toast — maturity honesty, surfaced in the UI.
const PENDING: &str = "PENDING_DAEMON: ";

// ---- token custody ----------------------------------------------------------

/// Absolute path of the operator token file (OS config dir / cerberus /
/// operator.token). Mirrors the Go `auth.OperatorTokenPath` layout.
fn operator_token_path() -> Result<PathBuf, String> {
    dirs::config_dir()
        .map(|d| d.join("cerberus").join("operator.token"))
        .ok_or_else(|| "cannot locate OS config dir".to_string())
}

/// Read the operator capability token from disk, trimmed. Absence means the
/// daemon has not run (or wrote it elsewhere) — treated as "disconnected".
fn operator_token() -> Result<String, String> {
    let path = operator_token_path()?;
    std::fs::read_to_string(&path)
        .map(|s| s.trim().to_string())
        .map_err(|_| "operator token not found (is cerberusd running?)".to_string())
}

// ---- small HTTP helpers -----------------------------------------------------

/// Build a ureq agent with bounded timeouts so a stalled daemon never hangs the
/// UI thread.
fn agent() -> ureq::Agent {
    ureq::AgentBuilder::new()
        .timeout_connect(HTTP_TIMEOUT)
        .timeout_read(HTTP_TIMEOUT)
        .timeout_write(HTTP_TIMEOUT)
        .build()
}

/// Map a ureq error into a message, distinguishing "route not served yet" (404,
/// -> PENDING) from a genuine connection/other failure.
fn http_err(context: &str, e: ureq::Error) -> String {
    match e {
        ureq::Error::Status(404, _) => format!("{PENDING}{context} not exposed by the daemon"),
        ureq::Error::Status(code, resp) => {
            let body = resp.into_string().unwrap_or_default();
            let body = body.trim();
            if body.is_empty() {
                format!("{context}: HTTP {code}")
            } else {
                format!("{context}: HTTP {code} — {body}")
            }
        }
        // Transport error (daemon down / connection refused / timeout).
        other => format!("{context}: {other}"),
    }
}

/// Authenticated GET returning the raw response body as a String.
fn auth_get(url: &str, context: &str) -> Result<String, String> {
    let token = operator_token()?;
    agent()
        .get(url)
        .set("Authorization", &format!("Bearer {token}"))
        .call()
        .map_err(|e| http_err(context, e))?
        .into_string()
        .map_err(|e| e.to_string())
}

// ---- commands: live, real endpoints ----------------------------------------

/// Full daemon status snapshot (version, profile, kernel, uptime, mesh/peers,
/// power/lid, operator balance). Real endpoint, token-gated.
#[tauri::command]
fn daemon_status() -> Result<String, String> {
    auth_get(&status_url(), "status API")
}

/// Liveness + readiness probes (unauthenticated). Returns a small JSON object
/// `{ "healthz": bool, "readyz": bool, "reason": string }` so the UI can show a
/// precise "up but not ready" state (e.g. mesh still coming up).
#[tauri::command]
fn daemon_health() -> Result<String, String> {
    let a = agent();
    let healthz = a.get(&healthz_url()).call().is_ok();
    let (readyz, reason) = match a.get(&readyz_url()).call() {
        Ok(_) => (true, String::new()),
        Err(ureq::Error::Status(503, resp)) => (
            false,
            resp.into_string().unwrap_or_else(|_| "not ready".into()),
        ),
        Err(_) => (false, "unreachable".into()),
    };
    #[derive(Serialize)]
    struct Health {
        healthz: bool,
        readyz: bool,
        reason: String,
    }
    serde_json::to_string(&Health {
        healthz,
        readyz,
        reason: reason.trim().to_string(),
    })
    .map_err(|e| e.to_string())
}

/// Live metrics. Fetches the Prometheus text exposition from the daemon and
/// parses it into a flat `{name: value}` JSON map so the webview does not have
/// to parse Prometheus itself. Real endpoint, token-gated.
#[tauri::command]
fn daemon_metrics() -> Result<String, String> {
    let text = auth_get(&metrics_url(), "metrics")?;
    let parsed = parse_prometheus(&text);
    serde_json::to_string(&parsed).map_err(|e| e.to_string())
}

/// Parse Prometheus text exposition (v0.0.4) into name -> f64. We only handle
/// the daemon's own metric set: unlabeled counters/gauges, one value per line.
/// `# HELP` / `# TYPE` comment lines and blanks are skipped.
fn parse_prometheus(text: &str) -> BTreeMap<String, f64> {
    let mut out = BTreeMap::new();
    for line in text.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        // "<name> <value>" — the daemon emits no labels, so split on last space.
        if let Some((name, value)) = line.rsplit_once(char::is_whitespace) {
            let name = name.trim();
            // Skip any labeled series defensively (name would contain '{').
            if name.contains('{') {
                continue;
            }
            let v = match value.trim() {
                "+Inf" => f64::INFINITY,
                "-Inf" => f64::NEG_INFINITY,
                "NaN" => f64::NAN,
                other => other.parse().unwrap_or(f64::NAN),
            };
            out.insert(name.to_string(), v);
        }
    }
    out
}

/// Run a workload through the OpenAI-compatible gateway (the real "run" action).
/// `model` names the component/model to place; `prompt` is the single user
/// message. Returns the assistant output string on success. Token-gated ("exec").
#[tauri::command]
fn run_workload(model: String, prompt: String) -> Result<String, String> {
    let model = model.trim();
    let prompt = prompt.trim();
    if model.is_empty() {
        return Err("model is required".into());
    }
    if prompt.is_empty() {
        return Err("prompt is required".into());
    }
    let token = operator_token()?;
    let body = serde_json::json!({
        "model": model,
        "messages": [{ "role": "user", "content": prompt }],
    });
    let resp = agent()
        .post(&gateway_chat_url())
        .set("Authorization", &format!("Bearer {token}"))
        .set("Content-Type", "application/json")
        .send_json(body)
        .map_err(|e| http_err("gateway", e))?;
    let raw = resp.into_string().map_err(|e| e.to_string())?;
    // Extract choices[0].message.content for a clean result; fall back to raw.
    let parsed: serde_json::Value = serde_json::from_str(&raw).unwrap_or(serde_json::Value::Null);
    let content = parsed
        .get("choices")
        .and_then(|c| c.get(0))
        .and_then(|c| c.get("message"))
        .and_then(|m| m.get("content"))
        .and_then(|c| c.as_str())
        .map(|s| s.to_string());
    Ok(content.unwrap_or(raw))
}

/// Return the operator token so the webview can offer a "copy token" action.
/// The token already lives in plaintext on disk (0600); handing it to the local
/// webview does not widen the trust boundary (same machine, same user).
#[tauri::command]
fn operator_token_value() -> Result<String, String> {
    operator_token()
}

/// Return the absolute path of the operator token file, for display/diagnostics.
#[tauri::command]
fn operator_token_file() -> Result<String, String> {
    operator_token_path().map(|p| p.display().to_string())
}

// ---- commands: intended endpoints (honest PENDING until the daemon serves) --

/// Open belief-conflicts (CRDT contradictions awaiting human resolution).
/// The data exists in `daemon/state` but has no status-API route yet, so this
/// returns PENDING_DAEMON when the route 404s — surfaced calmly in the UI.
#[tauri::command]
fn belief_conflicts() -> Result<String, String> {
    auth_get(&conflicts_url(), "belief-conflicts")
}

/// Running / recent workloads. Same posture: the scheduler tracks these, but no
/// HTTP route lists them yet -> PENDING_DAEMON until the daemon exposes it.
#[tauri::command]
fn workloads() -> Result<String, String> {
    auth_get(&workloads_url(), "workloads")
}

/// Device (9P) namespace listing. Served by `daemon/ninep` over the 9P wire, not
/// yet mirrored to the status API -> PENDING_DAEMON until exposed.
#[tauri::command]
fn devices() -> Result<String, String> {
    auth_get(&devices_url(), "devices")
}

/// Resolve a belief-conflict by choosing the winning value. POSTs to the
/// intended route; PENDING_DAEMON until the daemon serves it.
#[tauri::command]
fn resolve_conflict(subject: String, winning: String) -> Result<String, String> {
    let token = operator_token()?;
    let body = serde_json::json!({ "subject": subject, "winning": winning });
    let resp = agent()
        .post(&resolve_conflict_url())
        .set("Authorization", &format!("Bearer {token}"))
        .set("Content-Type", "application/json")
        .send_json(body)
        .map_err(|e| http_err("resolve-conflict", e))?;
    resp.into_string().map_err(|e| e.to_string())
}

/// Revoke a capability by its token id. PENDING_DAEMON until the daemon serves
/// the route (revocation itself is real in `daemon/auth`; only the HTTP hook is
/// missing).
#[tauri::command]
fn revoke_capability(cap_id: String) -> Result<String, String> {
    let cap_id = cap_id.trim();
    if cap_id.is_empty() {
        return Err("capability id is required".into());
    }
    let token = operator_token()?;
    let body = serde_json::json!({ "id": cap_id });
    let resp = agent()
        .post(&revoke_url())
        .set("Authorization", &format!("Bearer {token}"))
        .set("Content-Type", "application/json")
        .send_json(body)
        .map_err(|e| http_err("revoke", e))?;
    resp.into_string().map_err(|e| e.to_string())
}

/// Grant / pool a device into the mesh namespace. PENDING_DAEMON until served.
#[tauri::command]
fn grant_device(path: String, rights: String) -> Result<String, String> {
    let path = path.trim();
    if path.is_empty() {
        return Err("device path is required".into());
    }
    let token = operator_token()?;
    let body = serde_json::json!({ "path": path, "rights": rights });
    let resp = agent()
        .post(&grant_device_url())
        .set("Authorization", &format!("Bearer {token}"))
        .set("Content-Type", "application/json")
        .send_json(body)
        .map_err(|e| http_err("grant-device", e))?;
    resp.into_string().map_err(|e| e.to_string())
}

// ---- bundled daemon (sidecar) -----------------------------------------------

/// Holds the bundled cerberusd child process so it can be terminated when the app
/// exits (otherwise closing the window would orphan a running daemon).
struct DaemonChild(Mutex<Option<CommandChild>>);

/// Launch the bundled cerberusd sidecar so the mesh is live the instant the app
/// opens — the whole point of the one-click install (no terminal, no separate
/// daemon). This is the SAME cmd/cerberusd binary a user could run by hand; the
/// installer just ships it (bundle.externalBin) and we spawn it here.
///
/// Safe to call unconditionally: if a daemon is already running, cerberusd's
/// single-instance lock makes this child exit immediately and the app simply
/// talks to the daemon that's already up.
fn spawn_daemon(app: &tauri::AppHandle) {
    let sidecar = match app.shell().sidecar("cerberusd") {
        Ok(cmd) => cmd,
        Err(e) => {
            eprintln!("cerberus: bundled daemon unavailable (run cerberusd manually): {e}");
            return;
        }
    };
    match sidecar.spawn() {
        Ok((mut rx, child)) => {
            app.state::<DaemonChild>().0.lock().unwrap().replace(child);
            // Drain the daemon's stdout/stderr so its pipe never fills and blocks
            // it (cerberusd is chatty). Discarded here — the daemon logs itself.
            tauri::async_runtime::spawn(async move { while rx.recv().await.is_some() {} });
        }
        Err(e) => eprintln!("cerberus: failed to launch bundled daemon: {e}"),
    }
}

// ---- entrypoint -------------------------------------------------------------

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_clipboard_manager::init())
        .plugin(tauri_plugin_shell::init())
        .manage(DaemonChild(Mutex::new(None)))
        .setup(|app| {
            spawn_daemon(&app.handle());
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            daemon_status,
            daemon_health,
            daemon_metrics,
            run_workload,
            operator_token_value,
            operator_token_file,
            belief_conflicts,
            workloads,
            devices,
            resolve_conflict,
            revoke_capability,
            grant_device,
        ])
        .build(tauri::generate_context!())
        .expect("error while building tauri application")
        .run(|app, event| {
            // Tear the bundled daemon down when the app quits so we never orphan it.
            if let RunEvent::Exit = event {
                if let Some(child) = app.state::<DaemonChild>().0.lock().unwrap().take() {
                    let _ = child.kill();
                }
            }
        });
}
