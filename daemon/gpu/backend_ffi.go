//go:build ffi

package gpu

import "github.com/hash066/cerberus/daemon/ffi"

// dispatch (-tags ffi): route to the Rust core's GPU dispatch (core/cabi
// cerberus_gpu_submit) — the physical GPU via wgpu when cabi is built
// `--features gpu` and an adapter is present, else the Rust software backend. On
// ANY FFI error we fall back to the pure-Go software backend so the caller still
// gets a real result rather than a failure (honest: the returned backend name
// records that the fallback ran, and why).
func dispatch(k Kernel, param float32, a, b []float32) ([]float32, string, error) {
	// Reject invalid requests the same way the software path does, before crossing
	// the FFI boundary.
	if err := validate(k, a, b); err != nil {
		return nil, "", err
	}
	out, backend, err := ffi.GpuSubmit(int(k), param, a, b)
	if err != nil {
		soft, serr := software(k, param, a, b)
		if serr != nil {
			return nil, "", serr
		}
		return soft, "cpu-software (ffi unavailable: " + err.Error() + ")", nil
	}
	return out, backend, nil
}
