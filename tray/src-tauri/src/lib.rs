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
use std::time::Duration;

use serde::Serialize;

// ---- daemon endpoints -------------------------------------------------------

const STATUS_URL: &str = "http://127.0.0.1:7777/api/v1/status";
const METRICS_URL: &str = "http://127.0.0.1:7779/metrics";
const HEALTHZ_URL: &str = "http://127.0.0.1:7779/healthz";
const READYZ_URL: &str = "http://127.0.0.1:7779/readyz";
const GATEWAY_CHAT_URL: &str = "http://127.0.0.1:8080/v1/chat/completions";

// Intended (not-yet-served) routes for actions the daemon has not surfaced over
// HTTP. Kept here so that the day the Go side adds them, the desktop lights up
// with zero UI changes. Until then these 404 and we report PENDING_DAEMON.
const CONFLICTS_URL: &str = "http://127.0.0.1:7777/api/v1/conflicts";
const RESOLVE_URL: &str = "http://127.0.0.1:7777/api/v1/conflicts/resolve";
const DEVICES_URL: &str = "http://127.0.0.1:7777/api/v1/devices";
const WORKLOADS_URL: &str = "http://127.0.0.1:7777/api/v1/workloads";
const REVOKE_URL: &str = "http://127.0.0.1:7777/api/v1/cap/revoke";
const GRANT_DEVICE_URL: &str = "http://127.0.0.1:7777/api/v1/devices/grant";

const HTTP_TIMEOUT: Duration = Duration::from_secs(5);

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
    auth_get(STATUS_URL, "status API")
}

/// Liveness + readiness probes (unauthenticated). Returns a small JSON object
/// `{ "healthz": bool, "readyz": bool, "reason": string }` so the UI can show a
/// precise "up but not ready" state (e.g. mesh still coming up).
#[tauri::command]
fn daemon_health() -> Result<String, String> {
    let a = agent();
    let healthz = a.get(HEALTHZ_URL).call().is_ok();
    let (readyz, reason) = match a.get(READYZ_URL).call() {
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
    let text = auth_get(METRICS_URL, "metrics")?;
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
        .post(GATEWAY_CHAT_URL)
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
    auth_get(CONFLICTS_URL, "belief-conflicts")
}

/// Running / recent workloads. Same posture: the scheduler tracks these, but no
/// HTTP route lists them yet -> PENDING_DAEMON until the daemon exposes it.
#[tauri::command]
fn workloads() -> Result<String, String> {
    auth_get(WORKLOADS_URL, "workloads")
}

/// Device (9P) namespace listing. Served by `daemon/ninep` over the 9P wire, not
/// yet mirrored to the status API -> PENDING_DAEMON until exposed.
#[tauri::command]
fn devices() -> Result<String, String> {
    auth_get(DEVICES_URL, "devices")
}

/// Resolve a belief-conflict by choosing the winning value. POSTs to the
/// intended route; PENDING_DAEMON until the daemon serves it.
#[tauri::command]
fn resolve_conflict(subject: String, winning: String) -> Result<String, String> {
    let token = operator_token()?;
    let body = serde_json::json!({ "subject": subject, "winning": winning });
    let resp = agent()
        .post(RESOLVE_URL)
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
        .post(REVOKE_URL)
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
        .post(GRANT_DEVICE_URL)
        .set("Authorization", &format!("Bearer {token}"))
        .set("Content-Type", "application/json")
        .send_json(body)
        .map_err(|e| http_err("grant-device", e))?;
    resp.into_string().map_err(|e| e.to_string())
}

// ---- entrypoint -------------------------------------------------------------

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_clipboard_manager::init())
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
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
