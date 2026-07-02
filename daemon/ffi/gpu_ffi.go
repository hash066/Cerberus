//go:build ffi

// GPU-dispatch binding (core/cabi cerberus_gpu_submit / cerberus_gpu_backend) —
// the Go side of core/runtime's GpuDispatch. Enabled only with `-tags ffi`; it
// shares the same cabi static lib as the other engine bindings (the #cgo
// directives live in kernel_ffi.go). The default (non-ffi) build uses the pure-Go
// software backend in daemon/gpu instead and never references this file.
package ffi

/*
#include "cerberus.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// GpuSubmit runs an element-wise f32 kernel via the Rust core and returns the
// result plus the backend that actually ran it. kernelID: 0=VectorAdd,
// 1=Saxpy(alpha=param), 2=ScalarMul(scalar=param). b is ignored for ScalarMul.
// The output length equals len(a).
//
// The recover() is defense-in-depth against a Go-side marshaling panic only; a
// panic inside the Rust call is caught Rust-side by cabi's catch_unwind guard.
func GpuSubmit(kernelID int, param float32, a, b []float32) (out []float32, backend string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			out, backend, err = nil, "", fmt.Errorf("ffi: gpu submit panicked: %v", rec)
		}
	}()
	n := len(a)
	out = make([]float32, n)
	var aPtr, bPtr, outPtr *C.float
	if n > 0 {
		aPtr = (*C.float)(unsafe.Pointer(&a[0]))
		outPtr = (*C.float)(unsafe.Pointer(&out[0]))
	}
	var bLen C.size_t
	if len(b) > 0 {
		bPtr = (*C.float)(unsafe.Pointer(&b[0]))
		bLen = C.size_t(len(b))
	}
	written := C.cerberus_gpu_submit(
		C.uint32_t(kernelID), C.float(param),
		aPtr, C.size_t(n), bPtr, bLen, outPtr, C.size_t(n),
	)
	if written < 0 {
		return nil, "", errors.New("ffi: gpu submit failed (bad request or backend error)")
	}
	return out[:int(written)], gpuBackend(), nil
}

// gpuBackend reports which backend cerberus_gpu_submit uses now: "gpu-wgpu" when
// cabi is built --features gpu and a physical adapter is present, else
// "cpu-software".
func gpuBackend() string {
	p := C.cerberus_gpu_backend()
	if p == nil {
		return "unknown"
	}
	return C.GoString(p)
}
