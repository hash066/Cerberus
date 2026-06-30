//! GPU-dispatch abstraction (ARCHITECTURE §5 "AI accel via MLX / wgpu+tinygrad").
//!
//! A worker that wants to offload an array kernel (vector-add, SAXPY, a matmul
//! tile, …) should not care whether the host has a discrete GPU, an integrated
//! one, or none at all. [`GpuDispatch`] is that seam: submit a [`KernelSource`]
//! plus input buffers, get output buffers back. Two real backends implement it:
//!
//! - [`SoftwareGpu`] — a **real CPU backend**. It actually computes the kernel on
//!   the host (no GPU, no driver), so it is fully testable headlessly and is the
//!   honest fallback on GPU-less machines. Always built.
//! - [`WgpuDispatch`] — a **real wgpu backend** behind the default-OFF `gpu`
//!   feature. It compiles a WGSL compute shader for the same kernel and runs it on
//!   a physical adapter. It *compiles* unconditionally (when the feature is on) but
//!   may legitimately find **no adapter** on a headless/GPU-less host, in which
//!   case [`WgpuDispatch::new`] returns [`GpuError::NoAdapter`]. That is labelled,
//!   not faked: we never pretend a GPU ran when none did (CLAUDE.md "maturity
//!   honesty"). The `gpu` feature is OFF by default so cabi/default builds never
//!   pull `wgpu`.
//!
//! Both backends implement the **same** [`KernelSource`] kernels and must produce
//! identical results — that is what makes the software path a faithful fallback
//! for the GPU path rather than a different computation.

use std::fmt;

/// Errors a [`GpuDispatch`] backend can return.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum GpuError {
    /// Input buffers had mismatched or unexpected lengths for the kernel.
    BadInputs(String),
    /// No GPU adapter was available (only meaningful for the real GPU backend on a
    /// headless / GPU-less host). The caller should fall back to [`SoftwareGpu`].
    NoAdapter,
    /// The backend failed while compiling/running the kernel.
    Backend(String),
}

impl fmt::Display for GpuError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            GpuError::BadInputs(m) => write!(f, "bad inputs: {m}"),
            GpuError::NoAdapter => write!(f, "no GPU adapter available"),
            GpuError::Backend(m) => write!(f, "gpu backend error: {m}"),
        }
    }
}

impl std::error::Error for GpuError {}

/// A compute kernel to dispatch. Each variant is an element-wise f32 kernel that
/// **both** backends implement identically (CPU directly; wgpu via a matching WGSL
/// shader), so results are backend-independent.
///
/// These are intentionally small, well-known kernels (the workhorses of a tensor
/// runtime) so the software path can be verified against a hand-computed result
/// and the GPU path checked against the software path.
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum KernelSource {
    /// `out[i] = a[i] + b[i]` over two equal-length input buffers.
    VectorAdd,
    /// `out[i] = alpha * x[i] + y[i]` (BLAS SAXPY) over two equal-length buffers.
    Saxpy { alpha: f32 },
    /// `out[i] = x[i] * scalar` over a single input buffer.
    ScalarMul { scalar: f32 },
}

impl KernelSource {
    /// How many input buffers this kernel consumes.
    fn arity(&self) -> usize {
        match self {
            KernelSource::VectorAdd | KernelSource::Saxpy { .. } => 2,
            KernelSource::ScalarMul { .. } => 1,
        }
    }
}

/// The GPU-dispatch seam: submit a kernel + input buffers, receive output buffers.
///
/// `inputs` are the kernel's input buffers in declared order (see each
/// [`KernelSource`] variant). The return is the single output buffer; its length
/// equals the (common) input length. Implementors must validate arity/length and
/// return [`GpuError::BadInputs`] rather than panicking on a malformed request.
pub trait GpuDispatch {
    /// Run `kernel` over `inputs`, returning the output buffer.
    fn submit(&self, kernel: &KernelSource, inputs: &[&[f32]]) -> Result<Vec<f32>, GpuError>;
}

/// Validate input arity + equal lengths, returning the common length. Shared by
/// both backends so they reject the same malformed requests identically.
fn validate(kernel: &KernelSource, inputs: &[&[f32]]) -> Result<usize, GpuError> {
    let want = kernel.arity();
    if inputs.len() != want {
        return Err(GpuError::BadInputs(format!(
            "kernel needs {want} input buffer(s), got {}",
            inputs.len()
        )));
    }
    let n = inputs[0].len();
    if let Some(bad) = inputs.iter().find(|b| b.len() != n) {
        return Err(GpuError::BadInputs(format!(
            "input buffers must be equal length; expected {n}, got {}",
            bad.len()
        )));
    }
    Ok(n)
}

/// Real **CPU/software** GPU-dispatch backend.
///
/// Computes each [`KernelSource`] directly on the host. No GPU, no driver, fully
/// deterministic and headless — the honest fallback (and the reference the wgpu
/// backend is checked against in tests).
#[derive(Debug, Clone, Copy, Default)]
pub struct SoftwareGpu;

impl SoftwareGpu {
    pub fn new() -> Self {
        Self
    }
}

impl GpuDispatch for SoftwareGpu {
    fn submit(&self, kernel: &KernelSource, inputs: &[&[f32]]) -> Result<Vec<f32>, GpuError> {
        let n = validate(kernel, inputs)?;
        let mut out = vec![0.0f32; n];
        match *kernel {
            KernelSource::VectorAdd => {
                let (a, b) = (inputs[0], inputs[1]);
                for i in 0..n {
                    out[i] = a[i] + b[i];
                }
            }
            KernelSource::Saxpy { alpha } => {
                let (x, y) = (inputs[0], inputs[1]);
                for i in 0..n {
                    out[i] = alpha * x[i] + y[i];
                }
            }
            KernelSource::ScalarMul { scalar } => {
                let x = inputs[0];
                for i in 0..n {
                    out[i] = x[i] * scalar;
                }
            }
        }
        Ok(out)
    }
}

#[cfg(feature = "gpu")]
mod wgpu_backend;
#[cfg(feature = "gpu")]
pub use wgpu_backend::WgpuDispatch;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn software_vector_add_is_correct() {
        let a = [1.0f32, 2.0, 3.0, 4.0];
        let b = [10.0f32, 20.0, 30.0, 40.0];
        let out = SoftwareGpu::new()
            .submit(&KernelSource::VectorAdd, &[&a, &b])
            .expect("vector add runs");
        assert_eq!(out, vec![11.0, 22.0, 33.0, 44.0]);
    }

    #[test]
    fn software_saxpy_is_correct() {
        // out = 2*x + y
        let x = [1.0f32, 2.0, 3.0];
        let y = [0.5f32, 0.5, 0.5];
        let out = SoftwareGpu
            .submit(&KernelSource::Saxpy { alpha: 2.0 }, &[&x, &y])
            .expect("saxpy runs");
        assert_eq!(out, vec![2.5, 4.5, 6.5]);
    }

    #[test]
    fn software_scalar_mul_is_correct() {
        let x = [1.0f32, -2.0, 3.5];
        let out = SoftwareGpu
            .submit(&KernelSource::ScalarMul { scalar: 3.0 }, &[&x])
            .expect("scalar mul runs");
        assert_eq!(out, vec![3.0, -6.0, 10.5]);
    }

    #[test]
    fn wrong_arity_is_rejected_not_panicked() {
        // VectorAdd needs 2 inputs; give it 1.
        let a = [1.0f32];
        let err = SoftwareGpu.submit(&KernelSource::VectorAdd, &[&a]);
        assert!(matches!(err, Err(GpuError::BadInputs(_))));
    }

    #[test]
    fn mismatched_lengths_are_rejected() {
        let a = [1.0f32, 2.0];
        let b = [1.0f32];
        let err = SoftwareGpu.submit(&KernelSource::VectorAdd, &[&a, &b]);
        assert!(matches!(err, Err(GpuError::BadInputs(_))));
    }

    #[test]
    fn empty_buffers_yield_empty_output() {
        let a: [f32; 0] = [];
        let b: [f32; 0] = [];
        let out = SoftwareGpu
            .submit(&KernelSource::VectorAdd, &[&a, &b])
            .unwrap();
        assert!(out.is_empty());
    }
}
