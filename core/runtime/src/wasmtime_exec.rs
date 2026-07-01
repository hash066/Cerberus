//! Wasmtime + Cranelift execution path (Phase F1).
//!
//! ARCHITECTURE §5/§6 and [03-compute-orchestration §6] name **Wasmtime +
//! Cranelift, Component Model, WASI P2** as the production runtime: it JITs to the
//! host ISA, sandboxes the guest, and lets typed values (records, lists, strings)
//! cross node boundaries through WIT interfaces. This module adds that engine
//! **alongside** the existing pure-Rust `wasmi` path in [`crate`] — it does not
//! replace it. A caller chooses a backend by picking an [`Executor`] impl:
//! [`crate::WasmExecutor`] (wasmi) or [`WasmtimeExecutor`] (this).
//!
//! ## What is real here (tested)
//! - [`WasmtimeExecutor`] runs a **core WASM module**: compiles with Cranelift,
//!   instantiates, calls a no-arg `entry -> i32` export, and returns the result
//!   little-endian — exactly the shard-result convention the wasmi path uses, so
//!   the two are drop-in interchangeable behind [`Executor`].
//! - **Host-provided imports**: in [`HostImport::Input`] mode the guest may import
//!   `host.input() -> i32` (the task input's first 4 bytes as an LE i32), the same
//!   import the wasmi `InputAwareWasmExecutor` test uses. This proves real
//!   host↔guest calls over Wasmtime, which is what the `gpu`/`topic`/`memory`
//!   capabilities will ride on once wired to the WIT world.
//! - **Component Model**: [`run_component`] instantiates a real WASM **component**
//!   (not a core module) via Wasmtime's component API and calls a typed export.
//!   The unit tests build the component from hand-written component-model text, so
//!   this exercises the genuine component path with no external toolchain.
//!
//! ## WASI Preview 2 — now a real, tested call ([`run_wasi_component`])
//! A component compiled against `wasi:cli`/`wasi:random`/`wasi:io` (WASI Preview
//! 2) imports the WASI-P2 world and **cannot** be instantiated by the import-free
//! [`run_component`] path: it needs a `wasmtime_wasi::WasiCtx` in the store and
//! `wasmtime_wasi::p2::add_to_linker_sync(&mut linker)` to satisfy those imports.
//! [`run_wasi_component`] does exactly that. It is exercised by
//! `tests/wasi_p2.rs` against a committed WASI-P2 component fixture
//! (`tests/fixtures/wasi_p2_fixture.wasm`, built with `cargo-component` for the
//! `wasm32-wasip2` target): the guest draws `wasi:random` bytes, writes a line to
//! `wasi:cli/stdout`, and returns `1337`, which the host asserts. This turns the
//! previously-documented WASI-P2 hook into a genuine call.
//!
//! [`wasi_p2_extension_point`] remains as the doc-only worked example of the exact
//! wiring (kept for reference; the live implementation is [`run_wasi_component`]).
//! The WASI path is gated behind the `wasmtime` feature (it pulls `wasmtime-wasi`),
//! so the cabi `default-features = false` staticlib never compiles it.

use wasmtime::{Config, Engine, Linker, Module, Store, StoreLimits, StoreLimitsBuilder};

use crate::{Executor, Task, TaskResult};

/// Resource-governance defaults for untrusted guest WASM on the Wasmtime backend —
/// mirrors the wasmi-side constants in [`crate`] so both backends enforce the same
/// bounds (ARCHITECTURE's "hypervisor for untrusted multi-agent workloads" premise:
/// a guest must never hang the calling thread or exhaust host memory).
///
/// ## Fuel
/// [`wasmtime::Config::consume_fuel`] charges an implementation-defined cost per unit
/// of work; [`DEFAULT_FUEL`] is generous against this crate's trivial shard tasks
/// while still tripping in well under a second for a runaway loop.
///
/// ## Memory / tables / instances
/// [`DEFAULT_MEMORY_LIMIT_BYTES`] is applied via [`wasmtime::StoreLimitsBuilder`] +
/// [`wasmtime::Store::limiter`] (the [`wasmtime::ResourceLimiter`] hook): a
/// `memory.grow` past the cap fails cleanly instead of the host allocating it.
pub const DEFAULT_FUEL: u64 = 10_000_000;

/// Default linear-memory cap per store: 64 MiB.
pub const DEFAULT_MEMORY_LIMIT_BYTES: usize = 64 * 1024 * 1024;

/// Builds a Wasmtime [`Engine`] with fuel metering turned on (the CPU-bounding half of
/// resource governance).
fn governed_engine() -> Engine {
    let mut config = Config::default();
    config.consume_fuel(true);
    Engine::new(&config).expect("default wasmtime config is always valid")
}

/// Builds the default [`StoreLimits`] (the memory-bounding half of resource
/// governance) per [`DEFAULT_MEMORY_LIMIT_BYTES`].
fn governed_limits() -> StoreLimits {
    StoreLimitsBuilder::new()
        .memory_size(DEFAULT_MEMORY_LIMIT_BYTES)
        .build()
}

/// Applies the default fuel budget to a freshly created, fuel-enabled store.
fn set_default_fuel<T>(store: &mut Store<T>) -> Result<(), String> {
    store
        .set_fuel(DEFAULT_FUEL)
        .map_err(|e| format!("set_fuel: {e}"))
}

/// Store data for [`WasmtimeExecutor::execute`]: the LE-i32 view of the task input
/// (what `host.input` reads) plus the [`StoreLimits`] the [`wasmtime::ResourceLimiter`]
/// hook reads from, so fuel *and* memory limits both apply.
struct ExecState {
    input: i32,
    limits: StoreLimits,
}

/// Which host imports the Wasmtime executor exposes to the guest core module.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub enum HostImport {
    /// No imports: the module must be self-contained (calls only its own funcs).
    #[default]
    None,
    /// Expose `host.input() -> i32`: the task input's first 4 bytes as an LE i32
    /// (0 if shorter). Lets a guest *consume* an upstream stage's resolved output,
    /// so a Wasmtime worker can sit in the same promise pipeline as a wasmi one.
    Input,
}

/// Real WebAssembly executor backed by **Wasmtime + Cranelift**.
///
/// Shaped to mirror [`crate::WasmExecutor`] so the two are interchangeable behind
/// [`Executor`]: same `Task` in, same `TaskResult` out, same "no-arg `entry`
/// returning i32, encoded little-endian" convention. The only knob is which host
/// imports the guest may use ([`HostImport`]).
#[derive(Clone, Copy, Debug, Default)]
pub struct WasmtimeExecutor {
    imports: HostImport,
}

impl WasmtimeExecutor {
    /// Executor for self-contained modules (no host imports).
    pub fn new() -> Self {
        Self {
            imports: HostImport::None,
        }
    }

    /// Executor that also provides `host.input() -> i32` to the guest.
    pub fn with_input_import() -> Self {
        Self {
            imports: HostImport::Input,
        }
    }

    /// Compile + instantiate + call the entry, returning the i32 shard result.
    ///
    /// Store data is the LE-i32 view of `t.input`, which the `host.input` import
    /// reads. Errors (compile/instantiate/missing-entry/trap) are returned as
    /// `Err(String)` so a bad task is a rejected promise, never a panic — matching
    /// the wasmi path's failure semantics.
    fn execute(&self, t: &Task) -> Result<i32, String> {
        let input_i32 = le_i32_prefix(&t.input);

        let engine = governed_engine();
        let module = Module::new(&engine, &t.wasm[..]).map_err(|e| format!("compile: {e}"))?;
        let mut store = Store::new(
            &engine,
            ExecState {
                input: input_i32,
                limits: governed_limits(),
            },
        );
        store.limiter(|state| &mut state.limits);
        set_default_fuel(&mut store)?;
        let mut linker = Linker::<ExecState>::new(&engine);

        if self.imports == HostImport::Input {
            linker
                .func_wrap(
                    "host",
                    "input",
                    |caller: wasmtime::Caller<'_, ExecState>| caller.data().input,
                )
                .map_err(|e| format!("link host.input: {e}"))?;
        }

        let instance = linker
            .instantiate(&mut store, &module)
            .map_err(|e| format!("instantiate: {e}"))?;
        let func = instance
            .get_typed_func::<(), i32>(&mut store, &t.entry)
            .map_err(|e| format!("no entry {:?}: {e}", t.entry))?;
        func.call(&mut store, ()).map_err(|e| format!("trap: {e}"))
    }
}

impl Executor for WasmtimeExecutor {
    fn run(&self, t: &Task) -> TaskResult {
        match self.execute(t) {
            Ok(v) => TaskResult::ok(t.task_id.clone(), v.to_le_bytes().to_vec()),
            Err(e) => TaskResult::err(t.task_id.clone(), e),
        }
    }
}

/// First 4 bytes of `input` as a little-endian i32, or 0 if shorter. The shard
/// I/O convention shared with the wasmi path ([`TaskResult::as_i32`]).
fn le_i32_prefix(input: &[u8]) -> i32 {
    if input.len() >= 4 {
        i32::from_le_bytes([input[0], input[1], input[2], input[3]])
    } else {
        0
    }
}

/// Run a WASM **component** (Component Model) through Wasmtime and call a no-arg
/// `name -> u32` typed export, returning its value.
///
/// This is the real component path ARCHITECTURE §6 calls for: `bytes` must be a
/// *component* (not a core module), instantiated via [`wasmtime::component`]. The
/// component here imports nothing; the WASI-P2 import set is the labeled extension
/// point documented in [`wasi_p2_extension_point`].
///
/// Returns `Err(String)` on any failure (not a component / missing export / wrong
/// signature / trap) so callers get a clean rejection instead of a panic.
pub fn run_component(bytes: &[u8], name: &str) -> Result<u32, String> {
    use wasmtime::component::{Component, Linker};

    let engine = governed_engine();
    let component =
        Component::new(&engine, bytes).map_err(|e| format!("component compile: {e}"))?;
    let linker = Linker::<StoreLimits>::new(&engine);
    let mut store = Store::new(&engine, governed_limits());
    store.limiter(|limits| limits);
    set_default_fuel(&mut store)?;
    let instance = linker
        .instantiate(&mut store, &component)
        .map_err(|e| format!("component instantiate: {e}"))?;
    let func = instance
        .get_typed_func::<(), (u32,)>(&mut store, name)
        .map_err(|e| format!("no component export {name:?}: {e}"))?;
    let (result,) = func
        .call(&mut store, ())
        .map_err(|e| format!("component trap: {e}"))?;
    // Component calls must run post-return before the next call / store reuse.
    func.post_return(&mut store)
        .map_err(|e| format!("post_return: {e}"))?;
    Ok(result)
}

/// Host state for the WASI Preview 2 store: the WASI context, the resource table the
/// WASI host implementations use to track open streams/handles, and the
/// [`StoreLimits`] the [`wasmtime::ResourceLimiter`] hook reads from so a WASI
/// component is bounded by the same memory cap as the other execution paths.
///
/// `cfg`-gated on the `wasmtime` feature (which now enables `wasmtime-wasi`).
struct WasiHost {
    ctx: wasmtime_wasi::WasiCtx,
    table: wasmtime::component::ResourceTable,
    limits: StoreLimits,
}

impl wasmtime_wasi::WasiView for WasiHost {
    fn ctx(&mut self) -> wasmtime_wasi::WasiCtxView<'_> {
        wasmtime_wasi::WasiCtxView {
            ctx: &mut self.ctx,
            table: &mut self.table,
        }
    }
}

/// Run a **WASI Preview 2 component** through Wasmtime and call a no-arg
/// `name -> u32` typed export, returning its value.
///
/// Unlike [`run_component`] (which only links the host funcs we define and so can
/// only run components that import nothing/`host.*`), this puts a real
/// `wasmtime_wasi::WasiCtx` in the store and calls
/// `wasmtime_wasi::p2::add_to_linker_sync` so the component's `wasi:*` imports
/// (random, clocks, io, cli/stdout, …) are satisfied by the host. That is the
/// production WASI-P2 wiring ARCHITECTURE §5/§6 calls for, made real.
///
/// `bytes` must be a *component* (not a core module). Guest stdout/stderr are
/// inherited so a fixture writing to `wasi:cli/stdout` is visible under
/// `cargo test -- --nocapture`. Any failure (not a component / missing export /
/// unsatisfied import / trap) is returned as `Err(String)` — never a panic.
pub fn run_wasi_component(bytes: &[u8], name: &str) -> Result<u32, String> {
    use wasmtime::component::{Component, Linker};
    use wasmtime_wasi::{WasiCtxBuilder, WasiView};

    let engine = governed_engine();
    let component =
        Component::new(&engine, bytes).map_err(|e| format!("component compile: {e}"))?;

    // Link the full WASI-P2 host surface into the component linker. This is the
    // step the import-free `run_component` path deliberately omits.
    let mut linker = Linker::<WasiHost>::new(&engine);
    wasmtime_wasi::p2::add_to_linker_sync(&mut linker)
        .map_err(|e| format!("add wasi to linker: {e}"))?;

    let host = WasiHost {
        ctx: WasiCtxBuilder::new()
            .inherit_stdout()
            .inherit_stderr()
            .build(),
        table: wasmtime::component::ResourceTable::new(),
        limits: governed_limits(),
    };
    let mut store = Store::new(&engine, host);
    store.limiter(|host| &mut host.limits);
    set_default_fuel(&mut store)?;

    let instance = linker
        .instantiate(&mut store, &component)
        .map_err(|e| format!("wasi component instantiate: {e}"))?;
    let func = instance
        .get_typed_func::<(), (u32,)>(&mut store, name)
        .map_err(|e| format!("no component export {name:?}: {e}"))?;
    let (result,) = func
        .call(&mut store, ())
        .map_err(|e| format!("component trap: {e}"))?;
    func.post_return(&mut store)
        .map_err(|e| format!("post_return: {e}"))?;
    // touch WasiView so the import stays used even if the trait method is only
    // reached through the linker (keeps `-D warnings` happy without an allow).
    let _ = store.data_mut().ctx();
    Ok(result)
}

/// **Doc-only worked example — WASI Preview 2 wiring.**
///
/// The live, tested implementation is [`run_wasi_component`]; this remains as a
/// compact reference for the exact production wiring.
///
/// A component compiled against `wasi:cli`/`wasi:io` imports the WASI-P2 world.
/// To run it, the production wiring is:
///
/// ```ignore
/// // Cargo.toml: wasmtime-wasi = "37"   (matches the pinned wasmtime)
/// use wasmtime::{Engine, Store};
/// use wasmtime::component::{Component, Linker};
/// use wasmtime_wasi::{WasiCtx, WasiCtxBuilder, WasiCtxView, WasiView};
/// use wasmtime::component::ResourceTable;
///
/// struct Host { ctx: WasiCtx, table: ResourceTable }
/// impl WasiView for Host {
///     fn ctx(&mut self) -> WasiCtxView<'_> {
///         WasiCtxView { ctx: &mut self.ctx, table: &mut self.table }
///     }
/// }
///
/// let engine = Engine::default();
/// let mut linker = Linker::<Host>::new(&engine);
/// wasmtime_wasi::p2::add_to_linker_sync(&mut linker)?;       // satisfies wasi:* imports
/// let host = Host { table: ResourceTable::new(),
///                   ctx: WasiCtxBuilder::new().inherit_stdio().build() };
/// let mut store = Store::new(&engine, host);
/// let component = Component::new(&engine, wasi_p2_component_bytes)?;
/// let instance = linker.instantiate(&mut store, &component)?;
/// // …call the world's `run` export…
/// ```
///
/// This is the same wiring [`run_wasi_component`] performs for real; it is kept
/// as a one-glance reference. Not faked — the live path is exercised by
/// `tests/wasi_p2.rs` (CLAUDE.md "maturity honesty").
#[doc(hidden)]
pub fn wasi_p2_extension_point() {
    // Intentionally empty: see [`run_wasi_component`] for the live implementation.
}

#[cfg(test)]
mod tests {
    use super::*;

    // A core module exporting `run` -> i32 returning 42.
    fn answer_wasm() -> Vec<u8> {
        wat::parse_str(r#"(module (func (export "run") (result i32) i32.const 42))"#).unwrap()
    }

    // A core module whose `run` returns `host.input() + addend`.
    fn add_wasm(addend: i32) -> Vec<u8> {
        wat::parse_str(format!(
            r#"(module
                 (import "host" "input" (func $input (result i32)))
                 (func (export "run") (result i32)
                   call $input
                   i32.const {addend}
                   i32.add))"#
        ))
        .unwrap()
    }

    // A module exporting `run` that never returns: `(loop (br 0))` is an unconditional
    // backward branch to itself.
    fn infinite_loop_wasm() -> Vec<u8> {
        wat::parse_str(r#"(module (func (export "run") (result i32) (loop (br 0)) i32.const 0))"#)
            .unwrap()
    }

    // A module with a 1-page memory (no declared maximum) whose `run` tries to grow it
    // by ~2.6 GiB worth of pages — far past DEFAULT_MEMORY_LIMIT_BYTES (64 MiB).
    fn memory_hog_wasm() -> Vec<u8> {
        wat::parse_str(
            r#"(module
                 (memory 1)
                 (func (export "run") (result i32)
                   i32.const 40000
                   memory.grow))"#,
        )
        .unwrap()
    }

    // A WASM *component* (not a core module) exporting `answer: func() -> u32`.
    // Hand-written component-model text: a core module supplies the function, and
    // the component canon-lifts it to the component-level export. `wat` parses the
    // `(component …)` form into a real component binary — no external toolchain.
    fn answer_component() -> Vec<u8> {
        wat::parse_str(
            r#"(component
                 (core module $m
                   (func (export "answer") (result i32) i32.const 1337))
                 (core instance $i (instantiate $m))
                 (func $a (result u32) (canon lift (core func $i "answer")))
                 (export "answer" (func $a)))"#,
        )
        .unwrap()
    }

    #[test]
    fn wasmtime_executes_core_module() {
        let r = WasmtimeExecutor::new().run(&Task::wasm(vec![1], answer_wasm()));
        assert!(r.ok, "error: {}", r.error);
        assert_eq!(r.as_i32(), Some(42));
    }

    #[test]
    fn wasmtime_missing_entry_is_error_not_panic() {
        let mut t = Task::wasm(vec![2], answer_wasm());
        t.entry = "nope".into();
        let r = WasmtimeExecutor::new().run(&t);
        assert!(!r.ok, "missing entry must reject, not panic");
    }

    #[test]
    fn wasmtime_infinite_loop_traps_on_fuel_exhaustion_instead_of_hanging() {
        // Without fuel metering this guest never returns, hanging the calling thread
        // forever. Resource governance must trap it in bounded real time.
        let start = std::time::Instant::now();
        let r = WasmtimeExecutor::new().run(&Task::wasm(vec![9], infinite_loop_wasm()));
        let elapsed = start.elapsed();
        assert!(!r.ok, "infinite loop must be rejected, not succeed");
        assert!(
            r.error.contains("fuel") || r.error.contains("trap"),
            "expected a fuel/trap error, got: {}",
            r.error
        );
        assert!(
            elapsed < std::time::Duration::from_secs(10),
            "fuel exhaustion must bound real time, took {elapsed:?}"
        );
    }

    #[test]
    fn wasmtime_memory_grow_past_cap_is_rejected_not_oom() {
        // memory.grow returns -1 (does not trap) when the limiter denies growth, so
        // the guest's `run` still returns cleanly with -1 rather than the host OOM-ing.
        let r = WasmtimeExecutor::new().run(&Task::wasm(vec![10], memory_hog_wasm()));
        assert!(
            r.ok,
            "grow-denied is a clean -1 return, not an executor error: {}",
            r.error
        );
        assert_eq!(
            r.as_i32(),
            Some(-1),
            "memory.grow past the cap must fail (-1), not succeed"
        );
    }

    #[test]
    fn run_component_traps_on_fuel_exhaustion() {
        // Component-Model path: an infinite-looping core func lifted to a component
        // export must also be bounded by fuel, not hang the calling thread.
        let comp = wat::parse_str(
            r#"(component
                 (core module $m
                   (func (export "spin") (result i32) (loop (br 0)) i32.const 0))
                 (core instance $i (instantiate $m))
                 (func $s (result u32) (canon lift (core func $i "spin")))
                 (export "spin" (func $s)))"#,
        )
        .unwrap();
        let start = std::time::Instant::now();
        let err = run_component(&comp, "spin").expect_err("infinite loop must trap, not succeed");
        assert!(
            err.contains("fuel") || err.contains("trap"),
            "expected a fuel/trap error, got: {err}"
        );
        assert!(
            start.elapsed() < std::time::Duration::from_secs(10),
            "fuel exhaustion must bound real time"
        );
    }

    #[test]
    fn wasmtime_rejects_bad_bytes() {
        let r = WasmtimeExecutor::new().run(&Task::wasm(vec![3], b"not wasm".to_vec()));
        assert!(!r.ok);
        assert!(r.error.contains("compile"), "got: {}", r.error);
    }

    #[test]
    fn wasmtime_host_import_threads_input() {
        // host.input() == 100, guest adds 23 -> 123. Proves a real host-provided
        // import over Wasmtime, and that a Wasmtime worker consumes upstream output
        // exactly like the wasmi InputAwareWasmExecutor.
        let mut t = Task::wasm(b"add".to_vec(), add_wasm(23));
        t.input = 100i32.to_le_bytes().to_vec();
        let r = WasmtimeExecutor::with_input_import().run(&t);
        assert!(r.ok, "error: {}", r.error);
        assert_eq!(r.as_i32(), Some(123));
    }

    #[test]
    fn wasmtime_host_import_absent_input_defaults_zero() {
        // No input bytes -> host.input() == 0, guest adds 7 -> 7.
        let r = WasmtimeExecutor::with_input_import().run(&Task::wasm(b"z".to_vec(), add_wasm(7)));
        assert!(r.ok, "error: {}", r.error);
        assert_eq!(r.as_i32(), Some(7));
    }

    #[test]
    fn wasmtime_executor_is_drop_in_for_promise_table() {
        // The whole point: a Wasmtime executor plugs into the existing PromiseTable
        // unchanged, so the Component-Model engine sits in the same CapTP pipeline.
        use crate::PromiseTable;
        let pt = PromiseTable::new(WasmtimeExecutor::new());
        let h = pt.dispatch(Task::wasm(b"p".to_vec(), answer_wasm()));
        assert_eq!(pt.resolve(h).unwrap().as_i32(), Some(42));
    }

    #[test]
    fn wasmtime_runs_real_component() {
        // Real Component-Model path: instantiate a component (not a core module)
        // and call its component-level export.
        let comp = answer_component();
        let v = run_component(&comp, "answer").expect("component runs");
        assert_eq!(v, 1337);
    }

    #[test]
    fn run_component_rejects_core_module() {
        // A core module is NOT a component; the component loader must reject it
        // cleanly rather than mis-running it.
        let core = answer_wasm();
        assert!(run_component(&core, "answer").is_err());
    }

    #[test]
    fn run_component_missing_export_is_error() {
        let comp = answer_component();
        assert!(run_component(&comp, "nope").is_err());
    }
}
