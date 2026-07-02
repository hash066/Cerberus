// Package gpu is the daemon's GPU-dispatch surface (ARCHITECTURE §5 "AI accel via
// wgpu"): submit a small element-wise f32 kernel + input buffers, get the result
// back. It mirrors core/runtime's GpuDispatch (the Rust side) so the two agree
// numerically.
//
// Two backends, selected at build time — and the result ALWAYS reports which one
// actually ran, so nothing is ever presented as "GPU" when a GPU did not run:
//
//   - default build: a REAL pure-Go software backend that computes on the CPU.
//     Works everywhere, needs no C toolchain and no GPU. Reported as "cpu-software".
//   - `-tags ffi`: routes to the Rust core (core/cabi cerberus_gpu_submit), which
//     runs the physical GPU via wgpu when cabi is built `--features gpu` and an
//     adapter is present ("gpu-wgpu"), else the Rust software backend. On any FFI
//     error it falls back to the pure-Go backend so a real result still comes back.
//
// A kernel never fabricates a result: an unsupported/invalid request errors.
package gpu

import "fmt"

// Kernel identifies an element-wise f32 kernel. The integer values are the ABI
// contract with core/cabi's cerberus_gpu_submit (0/1/2) — do not renumber.
type Kernel int

const (
	VectorAdd Kernel = 0 // out[i] = a[i] + b[i]
	Saxpy     Kernel = 1 // out[i] = alpha*a[i] + b[i]  (param = alpha)
	ScalarMul Kernel = 2 // out[i] = a[i] * scalar       (param = scalar; b ignored)
)

func (k Kernel) String() string {
	switch k {
	case VectorAdd:
		return "vector-add"
	case Saxpy:
		return "saxpy"
	case ScalarMul:
		return "scalar-mul"
	default:
		return fmt.Sprintf("kernel(%d)", int(k))
	}
}

// ParseKernel maps a CLI kernel name to a Kernel.
func ParseKernel(name string) (Kernel, bool) {
	switch name {
	case "vector-add", "vectoradd", "add":
		return VectorAdd, true
	case "saxpy":
		return Saxpy, true
	case "scalar-mul", "scalarmul", "scale":
		return ScalarMul, true
	default:
		return 0, false
	}
}

// arity is how many input buffers the kernel consumes.
func (k Kernel) arity() int {
	if k == ScalarMul {
		return 1
	}
	return 2
}

// validate rejects unknown kernels and mismatched input lengths (the same checks
// core/runtime's validate() makes, so both sides reject identically).
func validate(k Kernel, a, b []float32) error {
	if k < VectorAdd || k > ScalarMul {
		return fmt.Errorf("unknown kernel %d", int(k))
	}
	if k.arity() == 2 && len(a) != len(b) {
		return fmt.Errorf("%s needs equal-length inputs, got %d and %d", k, len(a), len(b))
	}
	return nil
}

// software runs the kernel on the CPU in pure Go — a real reference backend,
// numerically identical to core/runtime's SoftwareGpu (and its WGSL shaders). It
// is the default backend and the fallback for the FFI path.
func software(k Kernel, param float32, a, b []float32) ([]float32, error) {
	if err := validate(k, a, b); err != nil {
		return nil, err
	}
	n := len(a)
	out := make([]float32, n)
	switch k {
	case VectorAdd:
		for i := 0; i < n; i++ {
			out[i] = a[i] + b[i]
		}
	case Saxpy:
		for i := 0; i < n; i++ {
			out[i] = param*a[i] + b[i]
		}
	case ScalarMul:
		for i := 0; i < n; i++ {
			out[i] = a[i] * param
		}
	}
	return out, nil
}

// Dispatch runs kernel k over the input buffer(s) on the best available backend
// and returns the result plus the name of the backend that ACTUALLY ran it
// ("cpu-software", or "gpu-wgpu"/"rust-software" under -tags ffi). b is ignored
// for ScalarMul. It is the single entry point the RPC/CLI call.
func Dispatch(k Kernel, param float32, a, b []float32) (out []float32, backend string, err error) {
	return dispatch(k, param, a, b)
}
