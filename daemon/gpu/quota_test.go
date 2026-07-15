package gpu

import (
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
)

func TestEnforceQuotaAllowsWithinBounds(t *testing.T) {
	req := mesh.GpuRequest{KernelID: int(VectorAdd), A: []float32{1, 2}, B: []float32{3, 4}}
	grant := auth.Grant{Resource: mesh.MeshGpuResource("local")}
	grant.Resource.Quota = &contract.Quota{
		Bytes: inputBytes(req) + outputBytes(req),
		Flops: estimateFlops(req),
	}
	if err := enforceQuota(grant, req); err != nil {
		t.Fatalf("expected within quota: %v", err)
	}
}

func TestEnforceQuotaRejectsBytes(t *testing.T) {
	req := mesh.GpuRequest{KernelID: int(VectorAdd), A: []float32{1, 2, 3}, B: []float32{4, 5, 6}}
	grant := auth.Grant{Resource: mesh.MeshGpuResource("local")}
	grant.Resource.Quota = &contract.Quota{Bytes: 1}
	if err := enforceQuota(grant, req); err == nil {
		t.Fatal("expected bytes quota rejection")
	}
}

func TestMeshGpuResourcePath(t *testing.T) {
	r := mesh.MeshGpuResource("local")
	if r.Kind != contract.KindGPU || r.Path != "cerberus/local/mesh-gpu" {
		t.Fatalf("unexpected resource: %+v", r)
	}
}
