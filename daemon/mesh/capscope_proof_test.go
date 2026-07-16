package mesh

// capscope_proof_test.go pins the invariant that makes the word "capability"
// honest on the mesh's signed-cap path: A CAPABILITY IS SCOPED TO ITS RESOURCE.
//
// Every test here presents a cap that is entirely legitimate — correctly signed,
// self-issued by the authenticated peer, unexpired, unrevoked, and carrying
// exactly the right the gate demands. The ONLY thing wrong with it is that it
// names a DIFFERENT resource. Each must be refused.
//
// That is the whole difference between a capability and a signed permission slip,
// and before capscope.go every one of these attacks SUCCEEDED: the gates checked
// issuer, expiry, revocation and right, and never once compared grant.Resource to
// what they were guarding. Each test below was verified to FAIL against the
// unfixed gate (comment out the grantCoversResource call in the protocol's verify
// function and the substituted cap is accepted again) and to pass with it.
//
// The pairings are deliberate, not arbitrary — each is a real substitution that
// the shipping code made possible:
//
//	gpu     ← a mesh-compute cap   (both demand RightExec: THE headline bug)
//	compute ← a mesh-gpu cap       (the same confusion, mirrored)
//	audio   ← a mesh-shard cap     (RightWrite to store a shard opened a speaker)
//	shard   ← an audio cap         (RightWrite to play audio wrote to the store)
//	meta    ← a mesh-shard cap     (same KindFS, so ONLY the path distinguishes)
//	fabric  ← a mesh-meta cap      (RightRead to read metadata enumerated components)
//
// TestMeshScopeIsRefinementOfKernelScope additionally pins the relationship
// between this package's matcher and the stub CapKernel's, so the two can never
// drift into disagreeing about a resource they both admit.

import (
	"context"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/wasm"
)

// twoScopedNodes returns two connected mesh nodes on site "test" — the site every
// XResource("test") in this file names.
func twoScopedNodes(t *testing.T, ctx context.Context) (requester, server *Fabric) {
	t.Helper()
	return twoLlamaNodes(t, ctx)
}

// --- gpu ← compute: the headline cross-service substitution ------------------

// TestGpuDeniedWithCapForDifferentResource is the bug the lane was opened for.
//
// compute.go and gpu.go share verifySignedCap and BOTH require RightExec. Before
// the resource check, the two gates were therefore interchangeable: this exact
// cap — issued so a peer could run a sandboxed WASM workload — passed the GPU
// gate and drove the node's physical GPU backend.
func TestGpuDeniedWithCapForDifferentResource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, worker := twoScopedNodes(t, ctx)

	var handlerCalls int
	worker.ServeGpuSigned(func(_ context.Context, _ GpuRequest, _ auth.Grant) (GpuResult, error) {
		handlerCalls++
		return GpuResult{Output: []float32{1}, Backend: "cpu-software"}, nil
	}, SelfIssuerResolver, nowFn, nil)

	// A REAL, valid, RightExec capability — for mesh-compute, not mesh-gpu.
	env := capForResourceAs(t, requester, MeshComputeResource("test"), contract.RightExec, time.Hour)

	_, err := requester.RequestGpuSigned(ctx, worker.PeerID(),
		GpuRequest{KernelID: 0, A: []float32{1}, B: []float32{2}}, requester.PeerID(), env)
	if err == nil {
		t.Fatal("a RightExec capability minted for MeshComputeResource drove the GPU endpoint — " +
			"the gate is not resource-scoped, so any RightExec cap grants cross-node GPU access")
	}
	if handlerCalls != 0 {
		t.Fatalf("GpuHandler ran %d times on a cap for another resource — the gate is not fail-closed", handlerCalls)
	}
}

// --- compute ← gpu: the same confusion, mirrored -----------------------------

func TestComputeDeniedWithCapForDifferentResource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, worker := twoScopedNodes(t, ctx)

	var handlerCalls int
	worker.ServeComputeSigned(func(_ context.Context, task contract.ComputeTask, _ auth.Grant) (contract.ComputeResult, error) {
		handlerCalls++
		return contract.ComputeResult{TaskID: task.TaskID, OK: true}, nil
	}, SelfIssuerResolver, nowFn, nil, contract.RightExec, MeshComputeResource("test"))

	// Valid RightExec cap — for mesh-gpu, not mesh-compute.
	env := capForResourceAs(t, requester, MeshGpuResource("test"), contract.RightExec, time.Hour)

	task := contract.ComputeTask{TaskID: []byte("t"), Caps: [][]byte{env}}
	res, err := requester.RequestComputeSigned(ctx, worker.PeerID(), task, requester.PeerID(), contract.CapHandle(0))
	if err == nil && res.OK {
		t.Fatal("a RightExec capability minted for MeshGpuResource ran a WASM workload — " +
			"the compute gate is not resource-scoped")
	}
	if handlerCalls != 0 {
		t.Fatalf("SignedComputeHandler ran %d times on a cap for another resource — not fail-closed", handlerCalls)
	}
}

// --- audio ← shard -----------------------------------------------------------

// TestAudioDeniedWithCapForDifferentResource: a PLAY session requires RightWrite.
// So does storing a dfs shard. Before the resource check, a peer granted the
// right to place a shard on this node could instead push audio into its speakers.
func TestAudioDeniedWithCapForDifferentResource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoScopedNodes(t, ctx)

	srv := newEchoAudioServer(nil)
	server.ServeAudio(srv, SelfIssuerResolver, nowFn, nil)

	// Valid RightWrite cap — for the shard store, not for audio.
	env := capForResourceAs(t, requester, MeshShardResource("test"), contract.RightWrite, time.Hour)

	client := &byteAudioClient{send: []byte("mic-bytes")}
	err := requester.OpenAudioSession(server.PeerID(), AudioDirPlay, client, env, requester.PeerID())
	if err == nil {
		t.Fatal("a RightWrite capability minted for MeshShardResource opened an AUDIO PLAY session — " +
			"the gate is not resource-scoped, so a shard-placement cap reaches this machine's speakers")
	}
	// The live device must never have been touched.
	time.Sleep(200 * time.Millisecond)
	if srv.played() {
		t.Fatal("PlayIncoming ran despite a failed gate — no speaker may be opened before the cap verifies")
	}
}

// --- shard ← audio -----------------------------------------------------------

func TestShardDeniedWithCapForDifferentResource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, worker := twoScopedNodes(t, ctx)

	var putCalled, getCalled bool
	store := trackingShardServer{ShardServer: newMemShardServer(), putCalled: &putCalled, getCalled: &getCalled}
	worker.ServeShards(store, SelfIssuerResolver, nowFn, nil)

	// Valid RightWrite cap — for audio, not for shard placement.
	env := capForResourceAs(t, requester, AudioResource("test"), contract.RightWrite, time.Hour)

	err := requester.RequestPutShard(ctx, worker.PeerID(), dummyCID(t, 7).Bytes(), []byte("shard"), env, requester.PeerID())
	if err == nil {
		t.Fatal("a RightWrite capability minted for AudioResource wrote to the dfs shard store — " +
			"the shard gate is not resource-scoped")
	}
	if putCalled {
		t.Fatal("PutShard ran despite a failed gate — the local store must not be touched before the cap verifies")
	}
}

// --- meta ← shard: same Kind, so ONLY the path distinguishes them -------------

// TestMetaDeniedWithCapForDifferentResource is the sharpest of these. Both
// MeshMetaResource and MeshShardResource are contract.KindFS and both take
// RightWrite for their write op, so Kind and right are IDENTICAL: the path is the
// only thing that separates "may place an opaque shard" from "may rewrite this
// node's /cer/fs manifests". A gate that does not compare paths cannot tell them
// apart at all.
func TestMetaDeniedWithCapForDifferentResource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, worker := twoScopedNodes(t, ctx)

	store := &trackingMetaServer{}
	worker.ServeMeta(store, SelfIssuerResolver, nowFn, nil)

	if got, want := MeshMetaResource("test").Kind, MeshShardResource("test").Kind; got != want {
		t.Fatalf("precondition: meta/shard Kinds differ (%q vs %q); this test's point is that they do NOT", got, want)
	}

	// Valid RightWrite cap — for shard placement, not metadata.
	env := capForResourceAs(t, requester, MeshShardResource("test"), contract.RightWrite, time.Hour)

	err := requester.RequestPutMeta(ctx, worker.PeerID(), "/cer/fs/x", []byte(`{"k":1}`), env, requester.PeerID())
	if err == nil {
		t.Fatal("a RightWrite capability minted for MeshShardResource rewrote /cer/fs metadata — " +
			"the meta gate is not resource-scoped, and Kind alone cannot distinguish these")
	}
	if store.putCalled {
		t.Fatal("PutMeta ran despite a failed gate — the local meta store must not be touched before the cap verifies")
	}
}

// trackingMetaServer is a minimal MetaServer that records whether it was reached.
// meta.go had no test file at all before this one.
type trackingMetaServer struct {
	putCalled bool
	getCalled bool
}

func (m *trackingMetaServer) PutMeta(_ string, _ []byte) error { m.putCalled = true; return nil }
func (m *trackingMetaServer) GetMeta(_ string) ([]byte, bool)  { m.getCalled = true; return nil, false }
func (m *trackingMetaServer) ListMeta() ([]string, error)      { return nil, nil }

// --- component-fetch ← meta --------------------------------------------------

// TestComponentFetchDeniedWithCapForDifferentResource: the component gate demands
// proof of standing on THIS site's mesh fabric. "Membership" is a claim about a
// specific fabric, so a RightRead cap for anything else must not stand in for it —
// otherwise any read cap lets a peer enumerate this node's whole component store.
func TestComponentFetchDeniedWithCapForDifferentResource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoScopedNodes(t, ctx)

	// Seed a real component so the ONLY thing that can stop the fetch is the gate,
	// not a store miss.
	cstore := wasm.NewContentStore()
	c, err := cstore.Put([]byte{0x00, 0x61, 0x73, 0x6d, 4, 4, 4})
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	var fetchCalled bool
	store := trackingComponentSource{ComponentSource: cstore, called: &fetchCalled}
	server.ServeComponentFetch(store, SelfIssuerResolver, nowFn, nil)

	// Valid RightRead cap — for metadata, not for fabric membership.
	env := capForResourceAs(t, requester, MeshMetaResource("test"), contract.RightRead, time.Hour)

	if _, err := requester.RequestComponent(ctx, server.PeerID(), c, env, requester.PeerID()); err == nil {
		t.Fatal("a RightRead capability minted for MeshMetaResource fetched components — " +
			"the component-fetch gate is not resource-scoped")
	}
	if fetchCalled {
		t.Fatal("the local ComponentSource was consulted despite a failed gate — not fail-closed")
	}
}

// --- the matcher itself ------------------------------------------------------

// TestResourceMatchesRejectsPrefixConfusionAndWildcards pins the two ways a
// sloppier matcher would leak, both of which matter precisely BECAUSE the granted
// side of a mesh cap arrives off the wire under the presenter's control.
func TestResourceMatchesRejectsPrefixConfusionAndWildcards(t *testing.T) {
	want := MeshGpuResource("site-a")

	if !resourceMatches(want, want) {
		t.Fatal("exact match rejected")
	}
	// Prefix confusion: a naive strings.HasPrefix would accept these.
	if resourceMatches(contract.ResourceRef{Kind: want.Kind, Path: "cerberus/site-a/mesh-g"}, want) {
		t.Fatal("a cap for a PREFIX of the guarded path was accepted (prefix confusion)")
	}
	if resourceMatches(contract.ResourceRef{Kind: want.Kind, Path: "cerberus/site-a"}, want) {
		t.Fatal("a cap for an ancestor path was accepted — a mesh cap must not cover a subtree")
	}
	// Wildcards: the kernel's coversPath honors these; a wire-supplied grant must
	// not get to. matchKey("**", x) is true for EVERY x, so accepting this would
	// mean one self-issued cap opens every service on the node.
	if resourceMatches(contract.ResourceRef{Kind: want.Kind, Path: "**"}, want) {
		t.Fatal("a wildcard '**' grant was accepted — one self-issued cap would open every KindGPU service")
	}
	if resourceMatches(contract.ResourceRef{Kind: want.Kind, Path: "cerberus/**"}, want) {
		t.Fatal("a wildcard subtree grant was accepted")
	}
	// Right path, wrong kind.
	if resourceMatches(contract.ResourceRef{Kind: contract.KindFS, Path: want.Path}, want) {
		t.Fatal("a cap of the wrong Kind was accepted")
	}
	// Zero grant.
	if resourceMatches(contract.ResourceRef{}, want) {
		t.Fatal("a zero-resource grant was accepted")
	}
	// Quota must be ignored: daemon/gpu/remote.go mints MeshGpuResource(site) and
	// then sets .Quota. Comparing it would deny every real GPU dispatch.
	withQuota := MeshGpuResource("site-a")
	withQuota.Quota = &contract.Quota{Bytes: 1 << 20, Flops: 1000}
	if !resourceMatches(withQuota, want) {
		t.Fatal("a cap carrying a Quota was rejected — Quota is a bound on the ref, not part of which thing is named")
	}
}

// TestMeshScopeIsRefinementOfKernelScope pins the one-way relationship between
// this package's matcher and the stub CapKernel's hierarchical one, against the
// REAL kernel rather than a restatement of it.
//
// The property: everything the mesh gate accepts, the kernel also accepts. Never
// the reverse. The mesh rule is a REFINEMENT of the kernel's, not a rival — so
// the two can never drift into disagreeing about a resource they both admit,
// which is the actual risk of having a second matcher in the tree.
//
// The converse deliberately does NOT hold, and the wildcard/subtree rows below
// are exactly where it breaks. That is not divergence, it is the trust boundary:
// the kernel's granted side is minted by trusted in-process code (where a subtree
// grant is a feature — Compose mints cerberus/<site>/telemetry and publishes
// beneath it), while the mesh's granted side is supplied by the peer presenting
// it. Hierarchy is a feature on one side of that line and an escalation primitive
// on the other.
func TestMeshScopeIsRefinementOfKernelScope(t *testing.T) {
	guarded := MeshGpuResource("site-a")

	cases := []struct {
		name    string
		granted contract.ResourceRef
	}{
		{"exact", guarded},
		{"exact with quota", func() contract.ResourceRef {
			r := MeshGpuResource("site-a")
			r.Quota = &contract.Quota{Bytes: 1}
			return r
		}()},
		{"other site", MeshGpuResource("site-b")},
		{"other service same kind", MeshComputeResource("site-a")},
		{"wrong kind", contract.ResourceRef{Kind: contract.KindFS, Path: guarded.Path}},
		{"zero", contract.ResourceRef{}},
		{"ancestor subtree", contract.ResourceRef{Kind: guarded.Kind, Path: "cerberus/site-a"}},
		{"wildcard all", contract.ResourceRef{Kind: guarded.Kind, Path: "**"}},
		{"wildcard subtree", contract.ResourceRef{Kind: guarded.Kind, Path: "cerberus/**"}},
		{"prefix confusion", contract.ResourceRef{Kind: guarded.Kind, Path: "cerberus/site-a/mesh-g"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meshOK := resourceMatches(tc.granted, guarded)

			// Ask the REAL kernel the same question: mint a handle for the granted
			// resource, then Verify a request naming the guarded one. RightExec/"exec"
			// keeps the rights check satisfied so the verdict isolates SCOPE.
			k := stub.NewCapKernel()
			h, err := k.Mint(tc.granted, []contract.Right{contract.RightExec}, nil)
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			kernelOK := k.Verify(h, contract.Request{Op: "exec", Resource: guarded}, 0) == nil

			if meshOK && !kernelOK {
				t.Fatalf("mesh accepted a grant the kernel refuses (granted=%+v, guarded=%+v) — "+
					"the mesh matcher must be a REFINEMENT of the kernel's, never looser",
					tc.granted, guarded)
			}
		})
	}
}
