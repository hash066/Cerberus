package mesh

// compute_signed_test.go covers the signed-capability compute dispatch path
// (ServeComputeSigned / RequestComputeSigned / verifySignedCap /
// grantHasRight), which the coverage profile showed at 0% before this file:
// the existing compute_test.go only exercises the legacy shared-kernel
// ServeCompute/RequestCompute path. This is the cross-kernel authority path
// described in compute.go's header — a worker trusting a capability it did
// not mint, verified purely by the issuing node's signature — so it is
// squarely "anything touching revocation, capability checks" the task asks to
// prioritize.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/contract/go/stub"
)

// newTestSignedCap builds a SignedCap issuer over a fresh in-memory Ed25519
// key and returns it alongside its public key, so tests are self-contained and
// never touch disk.
func newTestSignedCap(t *testing.T) (*auth.SignedCap, ed25519.PublicKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ks, err := auth.NewMemoryKeyStore(seed)
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	sc := auth.NewSignedCap(ks)
	pub, err := ks.PublicKey()
	if err != nil {
		t.Fatalf("pubkey: %v", err)
	}
	return sc, pub
}

// TestSignedComputeRoundTrip proves the happy path end-to-end over the real
// mesh transport: a requester attaches a signed capability envelope (minted by
// an issuer that is NOT the worker's own kernel) naming an exec right on a GPU
// resource; the worker resolves the issuer's public key (as if exchanged at
// discovery), verifies the envelope, and only then runs the handler.
func TestSignedComputeRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	defer requester.Close()
	worker, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	defer worker.Close()
	if err := requester.Connect(ctx, worker.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	issuerSC, issuerPub := newTestSignedCap(t)
	var issuerPeerID contract.PeerID
	copy(issuerPeerID[:], issuerPub)

	// Mint against the resource the worker's gate actually guards. This used to be
	// an arbitrary "/cer/dev/gpu/0", which proved nothing: the gate ignored
	// grant.Resource entirely, so EVERY cap passed and the "valid cap is allowed"
	// half of this test was vacuous.
	grant, err := auth.NewGrant(
		MeshComputeResource("test"),
		[]contract.Right{contract.RightExec},
		nil, time.Hour)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	env, err := issuerSC.Issue(grant)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// The worker's issuer resolver simulates having exchanged the issuer's
	// public key out of band at discovery.
	resolver := func(id contract.PeerID) (ed25519.PublicKey, bool) {
		if id == issuerPeerID {
			return issuerPub, true
		}
		return nil, false
	}

	var handlerCalled int32
	worker.ServeComputeSigned(func(_ context.Context, task contract.ComputeTask, g auth.Grant) (contract.ComputeResult, error) {
		handlerCalled++
		if g.Resource.Path != MeshComputeResource("test").Path {
			t.Errorf("handler saw wrong resource: %+v", g.Resource)
		}
		return contract.ComputeResult{TaskID: task.TaskID, OK: true, Output: []byte("ran")}, nil
	}, resolver, func() int64 { return time.Now().Unix() }, nil, contract.RightExec, MeshComputeResource("test"))

	task := contract.ComputeTask{TaskID: []byte("signed-t1"), Component: []byte("cid"), Caps: [][]byte{env}}
	res, err := requester.RequestComputeSigned(ctx, worker.PeerID(), task, issuerPeerID, contract.CapHandle(0))
	if err != nil {
		t.Fatalf("request compute signed: %v", err)
	}
	if !res.OK {
		t.Fatalf("worker denied a validly signed capability: %s", res.Error)
	}
	if string(res.Output) != "ran" {
		t.Fatalf("unexpected output %q", res.Output)
	}
	if handlerCalled != 1 {
		t.Fatalf("handler called %d times, want 1", handlerCalled)
	}
}

// TestSignedComputeRejectsUnknownIssuer proves the fail-closed gate: if the
// worker has no trusted key for the claimed issuer (never "met" it at
// discovery), the task is denied before the handler ever runs — no work
// happens on an envelope from an issuer the worker cannot verify.
func TestSignedComputeRejectsUnknownIssuer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	defer requester.Close()
	worker, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	defer worker.Close()
	if err := requester.Connect(ctx, worker.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	issuerSC, _ := newTestSignedCap(t)
	grant, err := auth.NewGrant(MeshComputeResource("test"), []contract.Right{contract.RightExec}, nil, time.Hour)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	env, err := issuerSC.Issue(grant)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	handlerCalled := false
	// resolver knows no issuer at all.
	resolver := func(contract.PeerID) (ed25519.PublicKey, bool) { return nil, false }
	worker.ServeComputeSigned(func(_ context.Context, task contract.ComputeTask, g auth.Grant) (contract.ComputeResult, error) {
		handlerCalled = true
		return contract.ComputeResult{TaskID: task.TaskID, OK: true}, nil
	}, resolver, func() int64 { return time.Now().Unix() }, nil, contract.RightExec, MeshComputeResource("test"))

	var randomIssuer contract.PeerID
	_, _ = rand.Read(randomIssuer[:])
	task := contract.ComputeTask{TaskID: []byte("t"), Caps: [][]byte{env}}
	res, err := requester.RequestComputeSigned(ctx, worker.PeerID(), task, randomIssuer, contract.CapHandle(0))
	if err != nil {
		t.Fatalf("request compute signed: %v", err)
	}
	if res.OK {
		t.Fatal("worker ran a task whose issuer it has no trusted key for")
	}
	if handlerCalled {
		t.Fatal("handler ran despite an unresolvable issuer — capability check is not fail-closed")
	}
}

// TestSignedComputeRejectsRevokedCap proves a capability that verifies
// cryptographically (valid signature, live window) but whose id the worker's
// revocation predicate flags is still denied — revocation is consulted BEFORE
// the handler runs, not advisory after the fact.
func TestSignedComputeRejectsRevokedCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	defer requester.Close()
	worker, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	defer worker.Close()
	if err := requester.Connect(ctx, worker.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	issuerSC, issuerPub := newTestSignedCap(t)
	var issuerPeerID contract.PeerID
	copy(issuerPeerID[:], issuerPub)

	grant, err := auth.NewGrant(MeshComputeResource("test"), []contract.Right{contract.RightExec}, nil, time.Hour)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	env, err := issuerSC.Issue(grant)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	verifiedOnce, err := auth.Verify(env, issuerPub, time.Now().Unix(), nil)
	if err != nil {
		t.Fatalf("pre-check verify: %v", err)
	}

	resolver := func(id contract.PeerID) (ed25519.PublicKey, bool) {
		if id == issuerPeerID {
			return issuerPub, true
		}
		return nil, false
	}
	// isRevoked flags exactly this grant's id — as if the worker heard a
	// revocation over gossip for this capability.
	isRevoked := func(id contract.CapID) bool { return id == verifiedOnce.ID }

	handlerCalled := false
	worker.ServeComputeSigned(func(_ context.Context, task contract.ComputeTask, g auth.Grant) (contract.ComputeResult, error) {
		handlerCalled = true
		return contract.ComputeResult{TaskID: task.TaskID, OK: true}, nil
	}, resolver, func() int64 { return time.Now().Unix() }, isRevoked, contract.RightExec, MeshComputeResource("test"))

	task := contract.ComputeTask{TaskID: []byte("t"), Caps: [][]byte{env}}
	res, err := requester.RequestComputeSigned(ctx, worker.PeerID(), task, issuerPeerID, contract.CapHandle(0))
	if err != nil {
		t.Fatalf("request compute signed: %v", err)
	}
	if res.OK {
		t.Fatal("worker ran a task carrying a revoked capability")
	}
	if handlerCalled {
		t.Fatal("handler ran despite the capability being revoked — revocation is not enforced before dispatch")
	}
}

// TestSignedComputeRejectsMissingRequiredRight proves that even a validly
// signed, unrevoked, un-expired capability is denied if it does not convey the
// right the worker requires for this handler (e.g. a read-only grant
// presented to an exec-only endpoint).
func TestSignedComputeRejectsMissingRequiredRight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	defer requester.Close()
	worker, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	defer worker.Close()
	if err := requester.Connect(ctx, worker.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	issuerSC, issuerPub := newTestSignedCap(t)
	var issuerPeerID contract.PeerID
	copy(issuerPeerID[:], issuerPub)

	// Grant conveys only RightRead, but the worker requires RightExec.
	grant, err := auth.NewGrant(MeshComputeResource("test"), []contract.Right{contract.RightRead}, nil, time.Hour)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	env, err := issuerSC.Issue(grant)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	resolver := func(id contract.PeerID) (ed25519.PublicKey, bool) {
		if id == issuerPeerID {
			return issuerPub, true
		}
		return nil, false
	}
	handlerCalled := false
	worker.ServeComputeSigned(func(_ context.Context, task contract.ComputeTask, g auth.Grant) (contract.ComputeResult, error) {
		handlerCalled = true
		return contract.ComputeResult{TaskID: task.TaskID, OK: true}, nil
	}, resolver, func() int64 { return time.Now().Unix() }, nil, contract.RightExec, MeshComputeResource("test"))

	task := contract.ComputeTask{TaskID: []byte("t"), Caps: [][]byte{env}}
	res, err := requester.RequestComputeSigned(ctx, worker.PeerID(), task, issuerPeerID, contract.CapHandle(0))
	if err != nil {
		t.Fatalf("request compute signed: %v", err)
	}
	if res.OK {
		t.Fatal("worker ran a task whose capability lacks the required right")
	}
	if handlerCalled {
		t.Fatal("handler ran despite the capability lacking RightExec")
	}
}

// TestSignedComputeRejectsMissingEnvelope proves a task carrying no signed
// capability at all (Caps empty/short) is denied by verifySignedCap rather
// than panicking on an out-of-range slice access.
func TestSignedComputeRejectsMissingEnvelope(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	defer requester.Close()
	worker, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	defer worker.Close()
	if err := requester.Connect(ctx, worker.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	resolver := func(contract.PeerID) (ed25519.PublicKey, bool) { return nil, false }
	worker.ServeComputeSigned(func(_ context.Context, task contract.ComputeTask, g auth.Grant) (contract.ComputeResult, error) {
		return contract.ComputeResult{TaskID: task.TaskID, OK: true}, nil
	}, resolver, func() int64 { return time.Now().Unix() }, nil, contract.RightExec, MeshComputeResource("test"))

	// No Caps at all.
	task := contract.ComputeTask{TaskID: []byte("t")}
	res, err := requester.RequestComputeSigned(ctx, worker.PeerID(), task, contract.PeerID{}, contract.CapHandle(0))
	if err != nil {
		t.Fatalf("request compute signed: %v", err)
	}
	if res.OK {
		t.Fatal("worker ran a task with no signed capability envelope at all")
	}
}

// TestVerifySignedCapNoResolver directly covers verifySignedCap's guard for a
// nil IssuerPubResolver — a configuration bug (ServeComputeSigned wired
// without a resolver) must fail closed, not panic.
func TestVerifySignedCapNoResolver(t *testing.T) {
	_, err := verifySignedCap([][]byte{[]byte("x")}, contract.PeerID{}, nil, nil, nil, "", MeshComputeResource("test"), "compute")
	if err == nil {
		t.Fatal("verifySignedCap with a nil resolver must return an error")
	}
}

// TestGrantHasRight directly covers the small helper used to enforce
// requiredRight, including the "absent" case.
func TestGrantHasRight(t *testing.T) {
	g := auth.Grant{Rights: []contract.Right{contract.RightRead, contract.RightExec}}
	if !grantHasRight(g, contract.RightExec) {
		t.Fatal("grantHasRight missed a right that is present")
	}
	if grantHasRight(g, contract.RightWrite) {
		t.Fatal("grantHasRight reported a right that is absent")
	}
	if grantHasRight(auth.Grant{}, contract.RightExec) {
		t.Fatal("grantHasRight reported a right present on an empty grant")
	}
}
