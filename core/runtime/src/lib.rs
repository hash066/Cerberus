//! Vertical 03 — Compute Orchestration (WASM, sharding, promise pipelining). Lane A.
//!
//! v0.1 executes **real WebAssembly** via `wasmi` (a pure-Rust engine — no JIT,
//! no MSVC, runs on any host). A task carries module bytes and is run by calling
//! a no-arg exported entry function returning an i32 "shard result". A
//! [`PromiseTable`] implements CapTP-style **promise pipelining**: a task is
//! dispatched and its result resolved later, downstream work can be enqueued
//! against upstream promise ids before they resolve, and a dependent task's
//! input is fed from its producers' **resolved outputs** (see [`Stage`]).
//!
//! Components are content-addressed: [`BlockStore`] / [`Cid`] back
//! ARCHITECTURE §3.4 `ComputeTask.component` (an IPLD-style CID), so a worker is
//! only ever fed bytes that verifiably hash to the id naming them.
//!
//! ## Backends
//! Two real WASM engines sit behind the same [`Executor`] seam, so a caller picks
//! one without changing dispatch code:
//! - [`WasmExecutor`] — **wasmi**, pure-Rust, no JIT/MSVC; the portable default.
//! - [`WasmtimeExecutor`] — **Wasmtime + Cranelift** (ARCHITECTURE §5/§6), JITs to
//!   the host ISA and carries the real **Component Model** path ([`run_component`])
//!   and the real **WASI Preview 2** path ([`run_wasi_component`], which links a
//!   `wasmtime_wasi::WasiCtx` so a component's `wasi:*` imports are satisfied).
//!
//! ## GPU dispatch
//! [`GpuDispatch`] is the seam for offloading a compute kernel + input buffers to
//! an accelerator (ARCHITECTURE §5 "AI accel"). [`SoftwareGpu`] is a real,
//! headless-testable CPU backend; [`WgpuDispatch`] (behind the default-OFF `gpu`
//! feature) is a real **wgpu** backend that runs a WGSL kernel on a physical GPU
//! when one is present.
//!
//! Next steps (docs/verticals/03-compute-orchestration.md): wire the Wasmtime
//! Component Model + WASI P2 host to the frozen `agent` WIT world, bind
//! [`GpuDispatch`] to the WIT `gpu` capability, and real cross-node promise
//! pipelining over CapTP.

mod blockstore;
pub use blockstore::{BlockStore, Cid};

#[cfg(feature = "wasmtime")]
pub mod wasmtime_exec;
#[cfg(feature = "wasmtime")]
pub use wasmtime_exec::{run_component, run_wasi_component, HostImport, WasmtimeExecutor};

pub mod gpu;
#[cfg(feature = "gpu")]
pub use gpu::WgpuDispatch;
pub use gpu::{GpuDispatch, GpuError, KernelSource, SoftwareGpu};

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;

use wasmi::{Config, Engine, Linker, Module, Store, StoreLimits, StoreLimitsBuilder};

/// Resource-governance defaults for untrusted guest WASM on the wasmi backend
/// (ARCHITECTURE's "hypervisor for untrusted multi-agent workloads" premise means a
/// guest must never be able to hang the calling thread or exhaust host memory).
///
/// ## Fuel
/// wasmi 1.1's fuel metering ([`wasmi::Config::consume_fuel`]) charges an
/// implementation-defined cost per bytecode step; there's no portable "N seconds"
/// unit, so [`DEFAULT_FUEL`] is picked generously against the handful-of-instructions
/// tasks in this crate's test suite while still being small enough that a runaway
/// loop (`(loop (br 0))`) traps almost immediately rather than spinning forever.
///
/// ## Memory
/// [`DEFAULT_MEMORY_LIMIT_BYTES`] caps how far a guest's linear memory may grow, via
/// [`wasmi::StoreLimitsBuilder::memory_size`] enforced through wasmi's
/// [`wasmi::ResourceLimiter`] hook ([`wasmi::Store::limiter`]): a `memory.grow` past
/// the cap is rejected (returns `-1`), the store is never actually asked to allocate it.
pub const DEFAULT_FUEL: u64 = 10_000_000;

/// Default linear-memory cap per store: 64 MiB. Generous for realistic shard tasks,
/// small enough that a malicious `memory.grow` cannot pressure host RAM before being
/// rejected.
pub const DEFAULT_MEMORY_LIMIT_BYTES: usize = 64 * 1024 * 1024;

/// Builds a wasmi [`Engine`] with fuel metering turned on (the CPU-bounding half of
/// resource governance; the memory half is applied per-`Store` via [`governed_limits`]
/// + [`Store::limiter`]).
fn governed_engine() -> Engine {
    let mut config = Config::default();
    config.consume_fuel(true);
    Engine::new(&config)
}

/// Builds the default [`StoreLimits`] (the memory-bounding half of resource
/// governance) per [`DEFAULT_MEMORY_LIMIT_BYTES`].
fn governed_limits() -> StoreLimits {
    StoreLimitsBuilder::new()
        .memory_size(DEFAULT_MEMORY_LIMIT_BYTES)
        .build()
}

/// Store data for a governed wasmi execution that also needs host-callable state `T`
/// (e.g. the `host.input` value): bundles the caller's `T` with the [`StoreLimits`]
/// the [`wasmi::ResourceLimiter`] hook reads from, so both fuel *and* memory limits
/// apply even when a task needs custom store data. See [`InputAwareWasmExecutor`]
/// (test-only) for the pattern; the equivalent shape is used by the wasmtime backend.
#[cfg(test)]
struct GovernedData<T> {
    inner: T,
    limits: StoreLimits,
}

#[cfg(test)]
impl<T> GovernedData<T> {
    fn new(inner: T) -> Self {
        Self {
            inner,
            limits: governed_limits(),
        }
    }
}

/// A unit of work handed to the runtime.
#[derive(Clone, Debug)]
pub struct Task {
    pub task_id: Vec<u8>,
    pub component: Vec<u8>, // IPLD CID label of the module
    pub wasm: Vec<u8>,      // the module bytes to execute
    pub entry: String,      // exported entry function (no args, returns i32)
    pub input: Vec<u8>,     // demo payload (echoed by EchoExecutor)
    pub caps: Vec<u64>,     // capability handles granted to this task
}

impl Task {
    /// Convenience constructor for a wasm task with the default `run` entry.
    pub fn wasm(task_id: Vec<u8>, wasm: Vec<u8>) -> Self {
        Task {
            task_id,
            component: Vec::new(),
            wasm,
            entry: "run".into(),
            input: Vec::new(),
            caps: Vec::new(),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TaskResult {
    pub task_id: Vec<u8>,
    pub ok: bool,
    pub output: Vec<u8>,
    pub error: String,
}

impl TaskResult {
    fn ok(task_id: Vec<u8>, output: Vec<u8>) -> Self {
        Self {
            task_id,
            ok: true,
            output,
            error: String::new(),
        }
    }
    fn err(task_id: Vec<u8>, error: String) -> Self {
        Self {
            task_id,
            ok: false,
            output: Vec::new(),
            error,
        }
    }
    /// Interpret the output as a little-endian i32 (the convention for wasm shard results).
    pub fn as_i32(&self) -> Option<i32> {
        if self.output.len() == 4 {
            Some(i32::from_le_bytes([
                self.output[0],
                self.output[1],
                self.output[2],
                self.output[3],
            ]))
        } else {
            None
        }
    }
}

/// The execution seam.
pub trait Executor: Send + Sync {
    fn run(&self, t: &Task) -> TaskResult;
}

/// Placeholder executor: returns the input as output (used by stubs/tests).
pub struct EchoExecutor;

impl Executor for EchoExecutor {
    fn run(&self, t: &Task) -> TaskResult {
        TaskResult::ok(t.task_id.clone(), t.input.clone())
    }
}

/// Real WebAssembly executor backed by wasmi.
#[derive(Default)]
pub struct WasmExecutor;

impl WasmExecutor {
    pub fn new() -> Self {
        Self
    }

    fn execute(t: &Task) -> Result<i32, String> {
        let engine = governed_engine();
        let module = Module::new(&engine, &t.wasm[..]).map_err(|e| format!("compile: {e}"))?;
        let mut store = Store::new(&engine, governed_limits());
        store.limiter(|limits| limits);
        store
            .set_fuel(DEFAULT_FUEL)
            .map_err(|e| format!("set_fuel: {e}"))?;
        let linker = Linker::<StoreLimits>::new(&engine);
        let instance = linker
            .instantiate_and_start(&mut store, &module)
            .map_err(|e| format!("instantiate: {e}"))?;
        let func = instance
            .get_typed_func::<(), i32>(&store, &t.entry)
            .map_err(|e| format!("no entry {:?}: {e}", t.entry))?;
        func.call(&mut store, ()).map_err(|e| format!("trap: {e}"))
    }
}

impl Executor for WasmExecutor {
    fn run(&self, t: &Task) -> TaskResult {
        match Self::execute(t) {
            Ok(v) => TaskResult::ok(t.task_id.clone(), v.to_le_bytes().to_vec()),
            Err(e) => TaskResult::err(t.task_id.clone(), e),
        }
    }
}

/// Opaque promise reference, mirrors contract `PromiseHandle`
/// (ARCHITECTURE §3.4 `Promise.promise_id`).
pub type PromiseHandle = u64;

/// How a dependent stage builds its input from its producers' resolved outputs.
///
/// CapTP promise pipelining means stage *N+1* is enqueued **before** stage *N*'s
/// bytes have landed; when they do, this function threads them into the dependent
/// task. `deps_outputs` is in the same order as the declared dependency handles.
/// The returned bytes become the dependent [`Task::input`].
type CombineFn = dyn Fn(&[Vec<u8>]) -> Vec<u8> + Send + Sync;

/// A pipelined stage: a task gated on a set of upstream promises, plus the rule
/// that turns those producers' resolved outputs into this task's input.
struct Stage {
    deps: Vec<PromiseHandle>,
    task: Task,
    combine: Box<CombineFn>,
}

/// Implements CapTP-style **promise pipelining** with real output-threading.
///
/// - [`dispatch`](PromiseTable::dispatch) runs a task now and returns a resolved
///   promise.
/// - [`dispatch_pipelined`](PromiseTable::dispatch_pipelined) enqueues a task
///   that depends on one or more upstream promises; it does **not** run until all
///   of them resolve, and when it does its input is computed from their resolved
///   outputs (so stage 2's input is stage 1's output — chained, not re-run blind).
/// - [`pump`](PromiseTable::pump) drives ready stages to completion, including
///   multi-stage chains (resolving stage N can unblock stage N+1 in one call).
///
/// If any dependency **rejects** (resolves with `ok == false`), the dependent is
/// rejected too (promise-error contagion, per [03 §9]); it is not executed.
///
/// Thread-safe: all interior state is behind a `Mutex`, the id counter is atomic,
/// and `&self` methods may be called concurrently from multiple threads.
pub struct PromiseTable<E: Executor> {
    exec: E,
    next: AtomicU64,
    results: Mutex<HashMap<PromiseHandle, TaskResult>>,
    pending: Mutex<HashMap<PromiseHandle, Stage>>,
}

impl<E: Executor> PromiseTable<E> {
    pub fn new(exec: E) -> Self {
        Self {
            exec,
            next: AtomicU64::new(1),
            results: Mutex::new(HashMap::new()),
            pending: Mutex::new(HashMap::new()),
        }
    }

    fn fresh(&self) -> PromiseHandle {
        self.next.fetch_add(1, Ordering::SeqCst)
    }

    /// Dispatch a task now; the result is available immediately under the returned promise.
    pub fn dispatch(&self, t: Task) -> PromiseHandle {
        let h = self.fresh();
        let r = self.exec.run(&t);
        self.results.lock().unwrap().insert(h, r);
        h
    }

    /// Pipeline a task against a single upstream promise. The dependent runs once
    /// `dep` resolves; its `input` is left as-authored (the upstream output is not
    /// threaded in). Kept for callers that only need ordering, not data flow; for
    /// chained data flow use [`PromiseTable::dispatch_pipelined`].
    pub fn dispatch_after(&self, dep: PromiseHandle, t: Task) -> PromiseHandle {
        self.dispatch_pipelined(&[dep], t, |_outputs| Vec::new())
    }

    /// Enqueue a task that depends on `deps` (CapTP-style `ComputeTask.deps`). The
    /// task runs only after **every** dependency resolves; at that point `combine`
    /// is called with the producers' resolved outputs (in `deps` order) and its
    /// return value becomes the dependent task's [`Task::input`]. This is the real
    /// pipelining property: stage 2's input is computed from stage 1's output.
    ///
    /// Returns immediately with the dependent's promise handle (which is *not* yet
    /// resolved). Call [`PromiseTable::pump`] to drive ready stages.
    pub fn dispatch_pipelined<F>(
        &self,
        deps: &[PromiseHandle],
        task: Task,
        combine: F,
    ) -> PromiseHandle
    where
        F: Fn(&[Vec<u8>]) -> Vec<u8> + Send + Sync + 'static,
    {
        let h = self.fresh();
        self.pending.lock().unwrap().insert(
            h,
            Stage {
                deps: deps.to_vec(),
                task,
                combine: Box::new(combine),
            },
        );
        h
    }

    /// Drive all pending stages whose dependencies have resolved, to a fixpoint.
    /// Resolving one stage can unblock another, so this loops until no stage is
    /// ready — a single `pump()` therefore completes an arbitrarily deep chain.
    pub fn pump(&self) {
        loop {
            // Collect the stages whose every dependency is now resolved, removing
            // them from `pending` while holding both locks to stay consistent.
            let ready: Vec<(PromiseHandle, Stage)> = {
                let results = self.results.lock().unwrap();
                let mut pending = self.pending.lock().unwrap();
                let ids: Vec<PromiseHandle> = pending
                    .iter()
                    .filter(|(_, s)| s.deps.iter().all(|d| results.contains_key(d)))
                    .map(|(h, _)| *h)
                    .collect();
                ids.into_iter()
                    .map(|h| (h, pending.remove(&h).unwrap()))
                    .collect()
            };
            if ready.is_empty() {
                break;
            }
            for (h, stage) in ready {
                let result = self.run_stage(&stage);
                self.results.lock().unwrap().insert(h, result);
            }
        }
    }

    /// Run one ready stage: gather its producers' outputs, propagate any rejection,
    /// thread the combined input in, and execute.
    fn run_stage(&self, stage: &Stage) -> TaskResult {
        let results = self.results.lock().unwrap();
        let mut outputs = Vec::with_capacity(stage.deps.len());
        for dep in &stage.deps {
            match results.get(dep) {
                // Promise-error contagion: a rejected dependency rejects the dependent.
                Some(r) if !r.ok => {
                    return TaskResult::err(
                        stage.task.task_id.clone(),
                        format!("dependency promise {dep} rejected: {}", r.error),
                    );
                }
                Some(r) => outputs.push(r.output.clone()),
                // pump() only calls this once all deps are present, so this is unreachable
                // in practice; treat a missing dep defensively rather than panicking.
                None => {
                    return TaskResult::err(
                        stage.task.task_id.clone(),
                        format!("dependency promise {dep} unresolved"),
                    );
                }
            }
        }
        drop(results); // release before executing (executor may be slow)

        let mut task = stage.task.clone();
        task.input = (stage.combine)(&outputs);
        self.exec.run(&task)
    }

    /// Resolve a promise to its result, if available.
    pub fn resolve(&self, h: PromiseHandle) -> Option<TaskResult> {
        self.results.lock().unwrap().get(&h).cloned()
    }

    /// Whether a promise has resolved (vs. still pending).
    pub fn is_resolved(&self, h: PromiseHandle) -> bool {
        self.results.lock().unwrap().contains_key(&h)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // A module exporting `run` -> i32 returning 42.
    fn answer_wasm() -> Vec<u8> {
        wat::parse_str(r#"(module (func (export "run") (result i32) i32.const 42))"#).unwrap()
    }

    #[test]
    fn wasm_executes_and_returns_value() {
        let r = WasmExecutor.run(&Task::wasm(vec![1], answer_wasm()));
        assert!(r.ok, "error: {}", r.error);
        assert_eq!(r.as_i32(), Some(42));
    }

    #[test]
    fn missing_entry_is_an_error_not_a_panic() {
        let mut t = Task::wasm(vec![2], answer_wasm());
        t.entry = "nope".into();
        let r = WasmExecutor.run(&t);
        assert!(!r.ok);
    }

    // A module exporting `run` that never returns: `(loop (br 0))` is an unconditional
    // backward branch to itself, spinning forever with no host-visible progress.
    fn infinite_loop_wasm() -> Vec<u8> {
        wat::parse_str(
            r#"(module (func (export "run") (result i32) (loop (br 0)) i32.const 0))"#,
        )
        .unwrap()
    }

    #[test]
    fn infinite_loop_traps_on_fuel_exhaustion_instead_of_hanging() {
        // Without fuel metering this guest never returns, hanging the calling thread
        // forever. Resource governance must trap it in bounded real time.
        let start = std::time::Instant::now();
        let r = WasmExecutor.run(&Task::wasm(vec![9], infinite_loop_wasm()));
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

    // A module with a 1-page memory (no declared maximum) whose `run` tries to grow
    // it by ~2 GiB worth of pages — far past DEFAULT_MEMORY_LIMIT_BYTES (64 MiB).
    fn memory_hog_wasm() -> Vec<u8> {
        wat::parse_str(
            r#"(module
                 (memory 1)
                 (func (export "run") (result i32)
                   i32.const 40000 ;; ~2.6 GiB worth of 64 KiB pages
                   memory.grow))"#,
        )
        .unwrap()
    }

    #[test]
    fn memory_grow_past_cap_is_rejected_not_oom() {
        // memory.grow returns -1 (does not trap) when the limiter denies growth, so the
        // guest's `run` still returns cleanly with -1 rather than the host OOM-ing.
        let r = WasmExecutor.run(&Task::wasm(vec![10], memory_hog_wasm()));
        assert!(r.ok, "grow-denied is a clean -1 return, not an executor error: {}", r.error);
        assert_eq!(
            r.as_i32(),
            Some(-1),
            "memory.grow past the cap must fail (-1), not succeed"
        );
    }

    #[test]
    fn echo_executor_round_trips_input() {
        let mut t = Task::wasm(vec![3], Vec::new());
        t.input = b"ping".to_vec();
        let r = EchoExecutor.run(&t);
        assert!(r.ok);
        assert_eq!(r.output, b"ping");
    }

    #[test]
    fn promise_pipelining_runs_after_dependency() {
        let pt = PromiseTable::new(WasmExecutor);
        let up = pt.dispatch(Task::wasm(vec![1], answer_wasm()));
        // downstream enqueued before it could observe the upstream result
        let down = pt.dispatch_after(up, Task::wasm(vec![2], answer_wasm()));
        assert!(pt.resolve(down).is_none()); // not run yet
        pt.pump();
        let r = pt.resolve(down).expect("downstream resolved");
        assert_eq!(r.as_i32(), Some(42));
    }

    /// Real WASM executor that exposes the task's input to the guest as an
    /// imported host function `host.input() -> i32` (LE i32 of the first 4 input
    /// bytes, or 0). This lets a guest *consume* an upstream stage's output, so a
    /// pipeline genuinely chains data through real WebAssembly rather than faking
    /// it host-side. The guest exports `run() -> i32`.
    struct InputAwareWasmExecutor;

    impl Executor for InputAwareWasmExecutor {
        fn run(&self, t: &Task) -> TaskResult {
            let input_i32 = if t.input.len() >= 4 {
                i32::from_le_bytes([t.input[0], t.input[1], t.input[2], t.input[3]])
            } else {
                0
            };
            let engine = governed_engine();
            let module = match Module::new(&engine, &t.wasm[..]) {
                Ok(m) => m,
                Err(e) => return TaskResult::err(t.task_id.clone(), format!("compile: {e}")),
            };
            let mut store = Store::new(&engine, GovernedData::new(input_i32));
            store.limiter(|data| &mut data.limits);
            if let Err(e) = store.set_fuel(DEFAULT_FUEL) {
                return TaskResult::err(t.task_id.clone(), format!("set_fuel: {e}"));
            }
            let mut linker = Linker::<GovernedData<i32>>::new(&engine);
            linker
                .func_wrap(
                    "host",
                    "input",
                    |caller: wasmi::Caller<'_, GovernedData<i32>>| -> i32 { caller.data().inner },
                )
                .unwrap();
            let instance = match linker.instantiate_and_start(&mut store, &module) {
                Ok(i) => i,
                Err(e) => return TaskResult::err(t.task_id.clone(), format!("instantiate: {e}")),
            };
            let func = match instance.get_typed_func::<(), i32>(&store, &t.entry) {
                Ok(f) => f,
                Err(e) => return TaskResult::err(t.task_id.clone(), format!("no entry: {e}")),
            };
            match func.call(&mut store, ()) {
                Ok(v) => TaskResult::ok(t.task_id.clone(), v.to_le_bytes().to_vec()),
                Err(e) => TaskResult::err(t.task_id.clone(), format!("trap: {e}")),
            }
        }
    }

    // Guest module: `run` returns `host.input() + addend`.
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

    #[test]
    fn two_stage_pipeline_chains_resolved_output() {
        // Stage 1: 0 + 100 = 100. Stage 2 reads stage 1's output and adds 23 -> 123.
        let pt = PromiseTable::new(InputAwareWasmExecutor);

        let stage1 = pt.dispatch(Task::wasm(b"s1".to_vec(), add_wasm(100)));

        // Stage 2 is enqueued BEFORE stage 1's bytes are observed; its input is
        // threaded from stage 1's resolved output via the combine closure.
        let stage2 = pt.dispatch_pipelined(
            &[stage1],
            Task::wasm(b"s2".to_vec(), add_wasm(23)),
            |outs| {
                // single producer -> forward its 4-byte i32 output as our input
                outs[0].clone()
            },
        );

        assert!(!pt.is_resolved(stage2), "stage 2 must wait for stage 1");
        pt.pump();

        let r1 = pt.resolve(stage1).expect("stage 1 resolved");
        assert_eq!(r1.as_i32(), Some(100));
        let r2 = pt.resolve(stage2).expect("stage 2 resolved");
        assert_eq!(
            r2.as_i32(),
            Some(123),
            "stage 2 input must be stage 1 output"
        );
    }

    #[test]
    fn three_stage_chain_resolves_in_one_pump() {
        // 0+10 -> +5 -> +1  ==> 16, exercising a deep chain and the fixpoint loop.
        let pt = PromiseTable::new(InputAwareWasmExecutor);
        let s1 = pt.dispatch(Task::wasm(b"a".to_vec(), add_wasm(10)));
        let s2 = pt.dispatch_pipelined(&[s1], Task::wasm(b"b".to_vec(), add_wasm(5)), |o| {
            o[0].clone()
        });
        let s3 = pt.dispatch_pipelined(&[s2], Task::wasm(b"c".to_vec(), add_wasm(1)), |o| {
            o[0].clone()
        });
        pt.pump();
        assert_eq!(pt.resolve(s3).unwrap().as_i32(), Some(16));
    }

    #[test]
    fn fan_in_combines_multiple_producers() {
        // Two independent producers (7 and 35); the dependent sums both outputs as
        // its input, then adds 0 -> 42. Proves multi-dep `deps` + combine ordering.
        let pt = PromiseTable::new(InputAwareWasmExecutor);
        let a = pt.dispatch(Task::wasm(b"a".to_vec(), add_wasm(7)));
        let b = pt.dispatch(Task::wasm(b"b".to_vec(), add_wasm(35)));
        let c = pt.dispatch_pipelined(&[a, b], Task::wasm(b"c".to_vec(), add_wasm(0)), |outs| {
            let x = i32::from_le_bytes([outs[0][0], outs[0][1], outs[0][2], outs[0][3]]);
            let y = i32::from_le_bytes([outs[1][0], outs[1][1], outs[1][2], outs[1][3]]);
            (x + y).to_le_bytes().to_vec()
        });
        pt.pump();
        assert_eq!(pt.resolve(c).unwrap().as_i32(), Some(42));
    }

    #[test]
    fn rejected_dependency_propagates() {
        // Stage 1 fails (bad wasm bytes); the dependent must be rejected, not run.
        let pt = PromiseTable::new(WasmExecutor);
        let bad = pt.dispatch(Task::wasm(b"bad".to_vec(), b"not wasm".to_vec()));
        assert!(!pt.resolve(bad).unwrap().ok);
        let down =
            pt.dispatch_pipelined(&[bad], Task::wasm(b"down".to_vec(), answer_wasm()), |_| {
                Vec::new()
            });
        pt.pump();
        let r = pt.resolve(down).expect("dependent resolved (as rejected)");
        assert!(!r.ok, "rejection must propagate to dependents");
        assert!(r.error.contains("rejected"));
    }

    #[test]
    fn blockstore_feeds_executor_via_cid() {
        // End-to-end: store a component by CID, fetch it back (integrity-checked),
        // and execute it. This is the §3.4 component-CID resolution path locally.
        let store = BlockStore::new();
        let cid = store.put(answer_wasm());
        let wasm = store.get(&cid).expect("component resolvable by CID");
        let r = WasmExecutor.run(&Task::wasm(b"t".to_vec(), wasm));
        assert_eq!(r.as_i32(), Some(42));
    }

    #[test]
    fn promise_table_is_thread_safe() {
        // Dispatch many tasks concurrently; every promise must resolve correctly.
        use std::sync::Arc;
        use std::thread;
        let pt = Arc::new(PromiseTable::new(WasmExecutor));
        let mut handles = Vec::new();
        for _ in 0..8 {
            let pt = Arc::clone(&pt);
            handles.push(thread::spawn(move || {
                let mut got = Vec::new();
                for _ in 0..25 {
                    let h = pt.dispatch(Task::wasm(b"x".to_vec(), answer_wasm()));
                    got.push(h);
                }
                got
            }));
        }
        let mut all = Vec::new();
        for h in handles {
            all.extend(h.join().unwrap());
        }
        // 8 * 25 = 200 distinct promise handles, each resolved to 42.
        all.sort_unstable();
        all.dedup();
        assert_eq!(all.len(), 200, "handles must be unique across threads");
        for h in all {
            assert_eq!(pt.resolve(h).unwrap().as_i32(), Some(42));
        }
    }
}
