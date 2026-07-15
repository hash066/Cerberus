package gpu

import (
	"fmt"

	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
)

// inputBytes is the f32 input buffer footprint for a kernel request.
func inputBytes(req mesh.GpuRequest) uint64 {
	n := uint64(len(req.A))
	if Kernel(req.KernelID) == ScalarMul {
		return n * 4
	}
	return n * 8 // two equal-length f32 buffers
}

// outputBytes is the f32 output buffer footprint for a kernel request.
func outputBytes(req mesh.GpuRequest) uint64 {
	return uint64(len(req.A)) * 4
}

// estimateFlops returns a conservative op count for micro-kernels (not ML).
func estimateFlops(req mesh.GpuRequest) uint64 {
	n := uint64(len(req.A))
	switch Kernel(req.KernelID) {
	case Saxpy:
		return n * 2 // mul + add per element
	default:
		return n
	}
}

// enforceQuota rejects a request whose buffer/FLOP footprint exceeds the grant's
// byte/FLOP quota bounds. A nil quota means unbounded (still RightExec-gated).
func enforceQuota(grant auth.Grant, req mesh.GpuRequest) error {
	q := grant.Resource.Quota
	if q == nil {
		return nil
	}
	needBytes := inputBytes(req) + outputBytes(req)
	if q.Bytes > 0 && needBytes > q.Bytes {
		return fmt.Errorf("gpu: bytes quota exceeded (need %d, cap %d)", needBytes, q.Bytes)
	}
	needFlops := estimateFlops(req)
	if q.Flops > 0 && needFlops > q.Flops {
		return fmt.Errorf("gpu: flops quota exceeded (need %d, cap %d)", needFlops, q.Flops)
	}
	return nil
}
