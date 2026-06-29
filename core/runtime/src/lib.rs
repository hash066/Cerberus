//! Vertical 03 — Compute Orchestration (WASM, sharding, promise pipelining). Lane A.
//!
//! v0.1 skeleton: an `Executor` trait + an `EchoExecutor` that "runs" a task by
//! echoing its input, so the 2-node demo path is exercisable before the real
//! wasmtime component host lands (docs/verticals/03-compute-orchestration.md).
//! The real host: wasmtime + component model + WASI P2, gpu-capability dispatch
//! to MLX/wgpu, CapTP promise pipelining.

/// A unit of work handed to the runtime. Mirrors contract ComputeTask minimally.
#[derive(Clone, Debug)]
pub struct Task {
    pub task_id: Vec<u8>,
    pub component: Vec<u8>, // CID of the WASM component
    pub input: Vec<u8>,
    pub caps: Vec<u64>, // capability handles granted to this task
}

#[derive(Clone, Debug)]
pub struct TaskResult {
    pub task_id: Vec<u8>,
    pub ok: bool,
    pub output: Vec<u8>,
    pub error: String,
}

/// The execution seam.
pub trait Executor {
    fn run(&self, t: &Task) -> TaskResult;
}

/// Placeholder executor used by the v0.1 demo: returns the input as output.
/// Replaced by the wasmtime component host.
pub struct EchoExecutor;

impl Executor for EchoExecutor {
    fn run(&self, t: &Task) -> TaskResult {
        TaskResult { task_id: t.task_id.clone(), ok: true, output: t.input.clone(), error: String::new() }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn echo_runs() {
        let r = EchoExecutor.run(&Task {
            task_id: vec![1],
            component: b"cid-hello".to_vec(),
            input: b"ping".to_vec(),
            caps: vec![],
        });
        assert!(r.ok);
        assert_eq!(r.output, b"ping");
    }
}
