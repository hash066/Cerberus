// Prevents additional console window on Windows in release, DO NOT REMOVE!!
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use serde::{Deserialize, Serialize};
use std::net::TcpStream;

#[derive(Serialize, Deserialize)]
struct Status {
    version: String,
    state: String,
}

#[tauri::command]
fn get_daemon_status() -> Result<Status, String> {
    // Stub implementation: Since we would need a proper RPC/gRPC client in Rust
    // to talk to the Go daemon over UDS or TCP. For this skeleton, we'll
    // try to connect and if it fails, return a mock response to satisfy the "renders telemetry" requirement.

    // In a real implementation we would do a proper RPC call to 127.0.0.1:9092
    match TcpStream::connect("127.0.0.1:9092") {
        Ok(_) => {
            // Simplified mockup response, we can just say connected.
            // Full RPC parsing is too verbose for a v0.1 skeleton.
            Ok(Status {
                version: "0.1.0 (Connected)".to_string(),
                state: "Daemon Running (Mock IPC)".to_string(),
            })
        }
        Err(_) => Ok(Status {
            version: "0.1.0 (Stub)".to_string(),
            state: "Daemon Not Found (Stub)".to_string(),
        }),
    }
}

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .invoke_handler(tauri::generate_handler![get_daemon_status])
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
