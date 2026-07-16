package compute_test

import (
	"context"
	"strings"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/compute"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/wasm"
	e2enode "github.com/hash066/cerberus/test/e2e/node"
)

// capScopeRig is a real two-fabric mesh with a real WireWorker on the worker
// side, so these tests exercise the ACTUAL signed-capability path
// (ServeComputeSigned -> verifySignedCap -> WireWorker's handler), not a mock.
type capScopeRig struct {
	requester *mesh.Fabric
	worker    *mesh.Fabric
	site      string
	hello     []byte
}

func newCapScopeRig(t *testing.T, ctx context.Context) *capScopeRig {
	t.Helper()
	const site = "cap-scope-test"

	requester, err := mesh.New(ctx, mesh.Config{Site: site, Kernel: stub.NewCapKernel(), EnableMDNS: false})
	if err != nil {
		t.Fatalf("requester mesh: %v", err)
	}
	t.Cleanup(func() { _ = requester.Close() })
	worker, err := mesh.New(ctx, mesh.Config{Site: site, Kernel: stub.NewCapKernel(), EnableMDNS: false})
	if err != nil {
		t.Fatalf("worker mesh: %v", err)
	}
	t.Cleanup(func() { _ = worker.Close() })
	if err := requester.Connect(ctx, worker.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	hello := e2enode.HelloShardWASM()
	if err := compute.WireWorker(worker, compute.WorkerConfig{
		Site:      site,
		Store:     wasm.NewContentStore(),
		Exec:      wasm.NewExecutor(hello),
		SeedBytes: hello,
	}); err != nil {
		t.Fatalf("wire worker: %v", err)
	}
	return &capScopeRig{requester: requester, worker: worker, site: site, hello: hello}
}

// issue mints a signed grant for an ARBITRARY resource/rights from the
// requester's own mesh identity — the same identity and signer the real
// PreferRemoteExecutor uses, so the envelope is genuinely valid. Only the
// resource it names varies.
func (r *capScopeRig) issue(t *testing.T, ref contract.ResourceRef, rights []contract.Right) ([]byte, contract.PeerID) {
	t.Helper()
	identity := r.requester.Identity()
	ks, err := auth.NewMemoryKeyStore(identity.Seed())
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	signer := auth.NewSignedCap(ks)
	g, err := auth.NewGrant(ref, rights, nil, time.Hour)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	env, err := signer.Issue(g)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	issuer, err := signer.IssuerPeerID()
	if err != nil {
		t.Fatalf("issuer id: %v", err)
	}
	return env, issuer
}

func (r *capScopeRig) dispatch(t *testing.T, ctx context.Context, env []byte, issuer contract.PeerID) (contract.ComputeResult, error) {
	t.Helper()
	c, err := wasm.ComponentCID(r.hello)
	if err != nil {
		t.Fatalf("cid: %v", err)
	}
	task := contract.ComputeTask{
		TaskID:    []byte("cap-scope"),
		Component: c.Bytes(),
		Caps:      [][]byte{env},
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.requester.RequestComputeSigned(rctx, r.worker.PeerID(), task, issuer, 0)
}

// TestComputeCapMustBeScopedToTheComputeResource is the compute-path analogue of
// the 9P device scoping fixed in 6475759 (a cap for the LOCAL cpu device must
// not open a PEER's device).
//
// The mesh compute worker verifies a presented capability's issuer, signature,
// validity window, revocation status, and that it carries RightExec. It did NOT
// verify WHAT RESOURCE the grant names — mesh.verifySignedCap has no resource
// check at all, and compute.WireWorker's handler received the verified grant and
// discarded it (`_ = grant`).
//
// So ANY signed grant carrying RightExec authorized arbitrary WASM execution on
// this node: a grant for a GPU device, for an unrelated 9P path, or for a
// DIFFERENT SITE's mesh-compute resource. That is authority derived from holding
// any exec-bearing cap rather than from holding a cap for THIS resource — the
// same ambient-authority shape, one layer up from the 9P namespace.
//
// Each subtest presents a genuinely valid, correctly signed envelope from a
// trusted issuer. The ONLY thing wrong is the resource it names.
func TestComputeCapMustBeScopedToTheComputeResource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rig := newCapScopeRig(t, ctx)

	t.Run("correctly scoped cap is accepted", func(t *testing.T) {
		env, issuer := rig.issue(t, mesh.MeshComputeResource(rig.site), []contract.Right{contract.RightExec})
		res, err := rig.dispatch(t, ctx, env, issuer)
		if err != nil {
			t.Fatalf("correctly scoped cap should run: %v", err)
		}
		if !res.OK {
			t.Fatalf("correctly scoped cap should run: %s", res.Error)
		}
	})

	// Every case below must be REFUSED. Before the fix all of them executed.
	wrong := []struct {
		name string
		ref  contract.ResourceRef
	}{
		{
			// A cap for this node's GPU device. Exec on a GPU is a real thing to
			// grant; it must not also buy CPU/WASM execution.
			name: "cap for a GPU device",
			ref:  contract.ResourceRef{Kind: contract.KindGPU, Path: "/cer/dev/gpu/local/0"},
		},
		{
			// A cap for a pooled CPU DEVICE is not a cap to execute code on the
			// mesh compute endpoint — different resource, different authority.
			name: "cap for a CPU device path",
			ref:  contract.ResourceRef{Kind: contract.KindCPU, Path: "/cer/dev/cpu/local/0"},
		},
		{
			// The most dangerous one: a legitimate mesh-compute cap for ANOTHER
			// SITE. Sites are the tenancy boundary; this crosses it.
			name: "mesh-compute cap for a DIFFERENT site",
			ref:  mesh.MeshComputeResource("some-other-site"),
		},
		{
			// Right kind, right shape, wrong path entirely.
			name: "cap for an unrelated topic",
			ref:  contract.ResourceRef{Kind: contract.KindTopic, Path: "cerberus/" + "cap-scope-test" + "/telemetry"},
		},
	}

	for _, tc := range wrong {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			env, issuer := rig.issue(t, tc.ref, []contract.Right{contract.RightExec})
			res, err := rig.dispatch(t, ctx, env, issuer)
			if err != nil {
				return // denied at the transport — refused, which is the point
			}
			if res.OK {
				t.Fatalf("SECURITY: a cap for %q (%s) executed WASM on the mesh compute "+
					"endpoint — the worker never checked the grant's resource",
					tc.ref.Path, tc.ref.Kind)
			}
			if !strings.Contains(strings.ToLower(res.Error), "resource") &&
				!strings.Contains(strings.ToLower(res.Error), "scope") &&
				!strings.Contains(strings.ToLower(res.Error), "denied") {
				t.Fatalf("refused, but not for the right reason: %q", res.Error)
			}
		})
	}
}

// TestComputeCapWithoutExecRightIsRefused pins the right check that already
// existed, so the resource check added alongside it cannot regress it.
func TestComputeCapWithoutExecRightIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rig := newCapScopeRig(t, ctx)

	// Correct resource, but only RightRead — no RightExec.
	env, issuer := rig.issue(t, mesh.MeshComputeResource(rig.site), []contract.Right{contract.RightRead})
	res, err := rig.dispatch(t, ctx, env, issuer)
	if err != nil {
		return
	}
	if res.OK {
		t.Fatal("SECURITY: a cap without RightExec executed WASM")
	}
}
