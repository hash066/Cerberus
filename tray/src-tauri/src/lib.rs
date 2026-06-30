// Cerberus tray app (Tauri v2). The Rust core performs the network call to the
// daemon's token-gated status API and hands JSON to the native webview, so the
// UI itself never makes network requests (consistent with the "no browser"
// boundary). The operator token is read from the same OS config path the daemon
// writes it to (auth.OperatorTokenPath on the Go side).
use std::path::PathBuf;

const STATUS_URL: &str = "http://127.0.0.1:7777/api/v1/status";

fn operator_token() -> Result<String, String> {
    let path: PathBuf = dirs::config_dir()
        .map(|d| d.join("cerberus").join("operator.token"))
        .ok_or_else(|| "cannot locate config dir".to_string())?;
    std::fs::read_to_string(&path)
        .map(|s| s.trim().to_string())
        .map_err(|_| "operator token not found (is cerberusd running?)".to_string())
}

/// Fetch the daemon status snapshot as JSON, authenticated with the operator
/// capability token. Returned to the webview for rendering.
#[tauri::command]
fn daemon_status() -> Result<String, String> {
    let token = operator_token()?;
    let resp = ureq::get(STATUS_URL)
        .set("Authorization", &format!("Bearer {token}"))
        .call()
        .map_err(|e| format!("daemon unreachable: {e}"))?;
    resp.into_string().map_err(|e| e.to_string())
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .invoke_handler(tauri::generate_handler![daemon_status])
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
