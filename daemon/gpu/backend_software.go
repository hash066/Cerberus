//go:build !ffi

package gpu

// dispatch (default build): the pure-Go software backend. No FFI, no GPU — real
// CPU compute, honestly reported. Build the daemon with `-tags ffi` (and cabi
// with `--features gpu`) to run kernels on a physical GPU via the Rust core.
func dispatch(k Kernel, param float32, a, b []float32) ([]float32, string, error) {
	out, err := software(k, param, a, b)
	if err != nil {
		return nil, "", err
	}
	return out, "cpu-software", nil
}
