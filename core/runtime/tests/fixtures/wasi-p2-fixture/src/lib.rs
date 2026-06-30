//! WASI-Preview-2 fixture guest for the cerberus-runtime WASI-P2 host test.
//!
//! Compiled against `wit/world.wit`, which imports `wasi:random/random` and
//! `wasi:cli/stdout`. Because those are real WASI-P2 imports, the produced
//! component cannot be instantiated without a `wasmtime_wasi::WasiCtx` and
//! `add_to_linker_sync` on the host side — that is the whole point: it forces the
//! host's documented WASI-P2 hook to become a real, exercised call.
//!
//! `run()` actually *uses* both imports (draws random bytes, writes a line to
//! stdout) and then returns the fixed shard result `1337` so the host test can
//! assert an exact value while still proving the WASI plumbing is live.
#[allow(warnings)]
mod bindings;

use bindings::wasi::cli::stdout::get_stdout;
use bindings::wasi::random::random::get_random_u64;
use bindings::Guest;

struct Component;

impl Guest for Component {
    fn run() -> u32 {
        // Exercise wasi:random — proves the random import is satisfied by the host.
        let r = get_random_u64();

        // Exercise wasi:cli/stdout — proves the stdout import is satisfied too.
        // (Value of `r` is folded into the message so the call isn't optimized out.)
        let out = get_stdout();
        let line = format!("wasi-p2-fixture: random-tag={:#018x}\n", r);
        let _ = out.blocking_write_and_flush(line.as_bytes());

        // Deterministic shard result so the host test asserts an exact value.
        1337
    }
}

bindings::export!(Component with_types_in bindings);
