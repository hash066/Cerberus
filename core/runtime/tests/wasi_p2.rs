//! WASI Preview 2 integration test.
//!
//! Runs a real WASI-P2 **component** (committed at
//! `tests/fixtures/wasi_p2_fixture.wasm`, built with `cargo-component` for the
//! `wasm32-wasip2` target — see `tests/fixtures/wasi-p2-fixture/`) through the
//! Wasmtime component path with a real `wasmtime_wasi::WasiCtx`. The fixture's
//! `run()` export draws `wasi:random` bytes and writes a line to `wasi:cli/stdout`
//! (so it genuinely exercises WASI imports the host must satisfy) and returns the
//! deterministic shard result `1337`, which this test asserts.
//!
//! This turns the previously-documented WASI-P2 hook in `wasmtime_exec.rs` into a
//! real, exercised call. Gated on the `wasmtime` feature (default-on); a no-op
//! placeholder test keeps `cargo test --no-default-features` green.

#![cfg(feature = "wasmtime")]

use cerberus_runtime::run_wasi_component;

/// The committed WASI-P2 component fixture.
const FIXTURE: &[u8] = include_bytes!("fixtures/wasi_p2_fixture.wasm");

#[test]
fn fixture_is_a_real_component() {
    // Component preamble: 0x00 0x61 0x73 0x6d, then layer/version 0x0d 0x00 0x01 0x00.
    // (A core module would be 0x01 0x00 0x00 0x00 — this distinguishes the two.)
    assert!(FIXTURE.len() > 8, "fixture must be non-trivial");
    assert_eq!(&FIXTURE[0..4], b"\0asm", "wasm magic");
    assert_eq!(
        &FIXTURE[4..8],
        &[0x0d, 0x00, 0x01, 0x00],
        "must be a component (not a core module)"
    );
}

#[test]
fn wasi_p2_component_runs_with_real_wasi_ctx() {
    // The fixture imports wasi:random + wasi:cli/stdout, so it can only run because
    // run_wasi_component links a real WasiCtx into the store. It returns 1337.
    let v = run_wasi_component(FIXTURE, "run").expect("WASI-P2 component runs");
    assert_eq!(v, 1337, "fixture run() must return the shard result 1337");
}

#[test]
fn missing_export_is_error_not_panic() {
    let err = run_wasi_component(FIXTURE, "does-not-exist");
    assert!(err.is_err(), "unknown export must be a clean error");
}

#[test]
fn import_free_path_cannot_run_a_wasi_component() {
    // The plain run_component (no WASI linked) must fail to instantiate a component
    // that imports wasi:* — this is exactly why run_wasi_component exists.
    let err = cerberus_runtime::run_component(FIXTURE, "run");
    assert!(
        err.is_err(),
        "a WASI-importing component must NOT instantiate without a WasiCtx"
    );
}
