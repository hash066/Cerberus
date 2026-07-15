// Package gpu wires mesh remote GPU dispatch for the production daemon:
// ServeGpuSigned registers during system composition so peers can run f32
// kernels on this node's best available backend (wgpu or cpu-software).
package gpu

import (
	"context"
	"fmt"
	"time"

	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
)

// WorkerConfig configures the mesh GPU worker registered on a Fabric.
type WorkerConfig struct {
	Site    string
	Revoked auth.RevocationPredicate
}

// WireWorker registers ServeGpuSigned on fab so remote peers can dispatch GPU
// kernels to this node after presenting a signed RightExec capability.
func WireWorker(fab *mesh.Fabric, cfg WorkerConfig) error {
	if fab == nil {
		return fmt.Errorf("gpu: nil fabric")
	}
	if cfg.Site == "" {
		cfg.Site = "local"
	}
	now := func() int64 { return time.Now().Unix() }
	fab.ServeGpuSigned(
		func(ctx context.Context, req mesh.GpuRequest, grant auth.Grant) (mesh.GpuResult, error) {
			if err := enforceQuota(grant, req); err != nil {
				return mesh.GpuResult{}, err
			}
			out, backend, err := Dispatch(Kernel(req.KernelID), req.Param, req.A, req.B)
			if err != nil {
				return mesh.GpuResult{}, err
			}
			return mesh.GpuResult{Output: out, Backend: backend}, nil
		},
		mesh.SelfIssuerResolver,
		now,
		cfg.Revoked,
	)
	return nil
}
