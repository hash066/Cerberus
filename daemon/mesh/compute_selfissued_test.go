package mesh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
)

// TestSelfIssuedComputeRequiresMatchingIssuer proves production-style mesh
// compute caps (issuer == authenticated remote peer) are stream-bound, while
// cross-issuer worker-granted caps (e2e discovery path) still work.
func TestSelfIssuedComputeRequiresMatchingIssuer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester, err := New(ctx, Config{Site: "self-issued", Kernel: stub.NewCapKernel(), EnableMDNS: false})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	defer requester.Close()
	worker, err := New(ctx, Config{Site: "self-issued", Kernel: stub.NewCapKernel(), EnableMDNS: false})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	defer worker.Close()
	if err := requester.Connect(ctx, worker.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	worker.ServeComputeSigned(
		func(_ context.Context, task contract.ComputeTask, g auth.Grant) (contract.ComputeResult, error) {
			return contract.ComputeResult{TaskID: task.TaskID, OK: true, Output: []byte("ok")}, nil
		},
		SelfIssuerResolver,
		func() int64 { return time.Now().Unix() },
		nil,
		contract.RightExec,
	)

	// Mint a self-issued cap under the requester's mesh identity.
	reqIdentity := requester.Identity()
	ks, err := auth.NewMemoryKeyStore(reqIdentity.Seed())
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	signer := auth.NewSignedCap(ks)
	issuerID, err := signer.IssuerPeerID()
	if err != nil {
		t.Fatalf("issuer id: %v", err)
	}
	g, err := auth.NewGrant(MeshComputeResource("self-issued"), []contract.Right{contract.RightExec}, nil, time.Hour)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	env, err := signer.Issue(g)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	task := contract.ComputeTask{TaskID: []byte("t"), Component: []byte("cid"), Caps: [][]byte{env}}
	res, err := requester.RequestComputeSigned(ctx, worker.PeerID(), task, issuerID, 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected ok, got error: %s", res.Error)
	}

	// Lie about the wire Issuer — should be denied.
	taskBad := contract.ComputeTask{TaskID: []byte("t2"), Component: []byte("cid"), Caps: [][]byte{env}}
	var wrongIssuer contract.PeerID
	if _, err := rand.Read(wrongIssuer[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	res, err = requester.RequestComputeSigned(ctx, worker.PeerID(), taskBad, wrongIssuer, 0)
	if err != nil {
		t.Fatalf("dispatch with wrong issuer frame: %v", err)
	}
	if res.OK {
		t.Fatal("expected denial when wire Issuer does not match self-issued grant")
	}
}

// TestWorkerGrantedComputeSkipsSelfIssuedBinding proves e2e-style caps where
// the worker is the issuer still work (grant.Issuer != remote requester).
func TestWorkerGrantedComputeSkipsSelfIssuedBinding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester, err := New(ctx, Config{Site: "e2e-style", Kernel: stub.NewCapKernel(), EnableMDNS: false})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	defer requester.Close()
	worker, err := New(ctx, Config{Site: "e2e-style", Kernel: stub.NewCapKernel(), EnableMDNS: false})
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

	worker.ServeComputeSigned(
		func(_ context.Context, task contract.ComputeTask, g auth.Grant) (contract.ComputeResult, error) {
			return contract.ComputeResult{TaskID: task.TaskID, OK: true, Output: []byte("ok")}, nil
		},
		func(id contract.PeerID) (ed25519.PublicKey, bool) {
			if id == issuerPeerID {
				return issuerPub, true
			}
			return nil, false
		},
		func() int64 { return time.Now().Unix() },
		nil,
		contract.RightExec,
	)

	grant, err := auth.NewGrant(
		contract.ResourceRef{Kind: contract.KindGPU, Path: "/cer/e2e/wasm/worker"},
		[]contract.Right{contract.RightExec},
		nil, time.Hour)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	env, err := issuerSC.Issue(grant)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	task := contract.ComputeTask{TaskID: []byte("t"), Caps: [][]byte{env}}
	res, err := requester.RequestComputeSigned(ctx, worker.PeerID(), task, issuerPeerID, 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !res.OK {
		t.Fatalf("worker-granted cap denied: %s", res.Error)
	}
}
