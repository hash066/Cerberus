//! Vertical 03 — Compute Orchestration (WASM, sharding, promise pipelining). Lane A.
//!
//! v0.1 executes **real WebAssembly** via `wasmi` (a pure-Rust engine — no JIT,
//! no MSVC, runs on any host). A task carries module bytes and is run by calling
//! a no-arg exported entry function returning an i32 "shard result". A
//! [`PromiseTable`] models CapTP-style promise pipelining: a task can be
//! dispatched and its result resolved later, and downstream work can be enqueued
//! against an upstream promise id before it resolves.
//!
//! Next steps (docs/verticals/03-compute-orchestration.md): the WASM Component
//! Model + WASI P2 host, `gpu`-capability dispatch to MLX/wgpu, and real
//! cross-node promise pipelining over CapTP.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;

use wasmi::{Engine, Linker, Module, Store};

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
        let engine = Engine::default();
        let module = Module::new(&engine, &t.wasm[..]).map_err(|e| format!("compile: {e}"))?;
        let mut store = Store::new(&engine, ());
        let linker = Linker::<()>::new(&engine);
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

/// Opaque promise reference, mirrors contract `PromiseHandle`.
pub type PromiseHandle = u64;

/// Models promise pipelining: dispatch returns a promise immediately; the result
/// is resolved later. Downstream tasks may be enqueued referencing an upstream
/// promise before it resolves (the latency-masking property), and are run when
/// their dependency resolves.
pub struct PromiseTable<E: Executor> {
    exec: E,
    next: AtomicU64,
    results: Mutex<HashMap<PromiseHandle, TaskResult>>,
    pending: Mutex<HashMap<PromiseHandle, (PromiseHandle, Task)>>, // promise -> (dep, task)
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

    /// Dispatch a task now; the result is available immediately under the returned promise.
    pub fn dispatch(&self, t: Task) -> PromiseHandle {
        let h = self.next.fetch_add(1, Ordering::SeqCst);
        let r = self.exec.run(&t);
        self.results.lock().unwrap().insert(h, r);
        h
    }

    /// Pipeline a task against an upstream promise: reserve a promise now, run the
    /// task only once `dep` has resolved (via [`PromiseTable::pump`]).
    pub fn dispatch_after(&self, dep: PromiseHandle, t: Task) -> PromiseHandle {
        let h = self.next.fetch_add(1, Ordering::SeqCst);
        self.pending.lock().unwrap().insert(h, (dep, t));
        h
    }

    /// Run any pending tasks whose dependency has resolved.
    pub fn pump(&self) {
        loop {
            let ready: Vec<(PromiseHandle, Task)> = {
                let results = self.results.lock().unwrap();
                let mut pending = self.pending.lock().unwrap();
                let ids: Vec<PromiseHandle> = pending
                    .iter()
                    .filter(|(_, (dep, _))| results.contains_key(dep))
                    .map(|(h, _)| *h)
                    .collect();
                ids.into_iter()
                    .map(|h| (h, pending.remove(&h).unwrap().1))
                    .collect()
            };
            if ready.is_empty() {
                break;
            }
            for (h, t) in ready {
                let r = self.exec.run(&t);
                self.results.lock().unwrap().insert(h, r);
            }
        }
    }

    /// Resolve a promise to its result, if available.
    pub fn resolve(&self, h: PromiseHandle) -> Option<TaskResult> {
        self.results.lock().unwrap().get(&h).cloned()
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
}
