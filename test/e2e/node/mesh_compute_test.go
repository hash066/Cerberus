package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log"
	"strconv"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/wasm"
)

func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(io.Discard, "", 0)
}

// newTestNode builds a minimal server backed by a real fabric + content store and
// its OWN Ed25519 issuer key, without the HTTP layer, so the cross-kernel SIGNED
// compute path can be unit-tested directly. It registers the signed-cap worker
// handler exactly as Run does.
func newTestNode(t *testing.T, ctx context.Context, id string) *server {
	t.Helper()
	k := stub.NewCapKernel()
	fab, err := mesh.New(ctx, mesh.Config{Site: meshSite, Kernel: k, EnableMDNS: false})
	if err != nil {
		t.Fatalf("%s mesh: %v", id, err)
	}
	t.Cleanup(func() { _ = fab.Close() })

	cstore := wasm.NewContentStore()
	if _, err := cstore.Put(HelloShardWASM()); err != nil {
		t.Fatalf("%s seed store: %v", id, err)
	}

	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("%s seed: %v", id, err)
	}
	keys, err := auth.NewMemoryKeyStore(seed)
	if err != nil {
		t.Fatalf("%s keystore: %v", id, err)
	}
	signer := auth.NewSignedCap(keys)
	pub, err := keys.PublicKey()
	if err != nil {
		t.Fatalf("%s pubkey: %v", id, err)
	}
	issuerID, err := signer.IssuerPeerID()
	if err != nil {
		t.Fatalf("%s issuer id: %v", id, err)
	}

	s := &server{
		self:        Peer{ID: id, MeshPeerID: encodePeerID(fab.PeerID()), IssuerPub: base64.StdEncoding.EncodeToString(pub)},
		kernel:      k,
		fabric:      fab,
		store:       cstore,
		log:         testLogger(t),
		signer:      signer,
		issuerPub:   pub,
		issuerID:    issuerID,
		peers:       map[string]Peer{},
		trustedKeys: map[contract.PeerID]ed25519.PublicKey{issuerID: pub},
	}
	fab.ServeComputeSigned(
		s.handleComputeSigned,
		s.resolveIssuerKey,
		func() int64 { return time.Now().Unix() },
		nil,
		contract.RightExec,
	)
	// Mirror Run(): answer peer component-fetch requests from our own store, so
	// tests built on this helper can exercise the peer-fetch fallback path.
	fab.ServeComponentFetch(cstore)
	return s
}

// trustPeer records another node's issuer pubkey as a trust anchor (what the
// HTTP discovery handshake does in production) so this node can Verify caps that
// peer minted.
func (s *server) trustPeer(t *testing.T, other *server) {
	t.Helper()
	if err := s.trustIssuer(other.self.IssuerPub); err != nil {
		t.Fatalf("trust %s issuer: %v", other.self.ID, err)
	}
}

// TestMeshComputeSignedRoundTrip runs the real dispatch path with a CROSS-KERNEL
// signed capability: the worker mints+signs an exec cap on ITS issuer key, the
// requester presents that envelope on dispatch, and the worker Verifies it against
// the issuer pubkey it exchanged — trusting a cap that (from the worker's view)
// travels the wire as a self-verifying token, not an opaque shared-kernel handle.
// It then resolves the CID from its store, runs the wasm, and returns 1337.
func TestMeshComputeSignedRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester := newTestNode(t, ctx, "requester")
	worker := newTestNode(t, ctx, "worker")

	// Requester trusts the worker's issuer key (exchanged at discovery in prod).
	requester.trustPeer(t, worker)

	if err := requester.fabric.Connect(ctx, worker.fabric.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Worker (resource owner) mints+signs the exec cap on its own issuer key.
	_, env, err := worker.grantExecCap()
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	// Requester content-addresses the component and dispatches the CID + signed cap.
	c, err := requester.store.Put(HelloShardWASM())
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	task := contract.ComputeTask{TaskID: []byte("task-1"), Component: c.Bytes(), Caps: [][]byte{env}}

	res, err := requester.fabric.RequestComputeSigned(ctx, worker.fabric.PeerID(), task, worker.issuerID, 0)
	if err != nil {
		t.Fatalf("request compute: %v", err)
	}
	if !res.OK {
		t.Fatalf("compute failed: %s", res.Error)
	}
	v, err := strconv.Atoi(string(res.Output))
	if err != nil {
		t.Fatalf("non-integer output %q: %v", res.Output, err)
	}
	if v != HelloShardValue {
		t.Fatalf("value = %d, want %d", v, HelloShardValue)
	}
}

// TestMeshComputeUnknownCID proves the worker rejects a task whose component CID
// is not in its content store (no unverifiable payload is executed), AFTER the
// signed cap has been verified.
func TestMeshComputeUnknownCID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	worker := newTestNode(t, ctx, "worker")
	_, env, err := worker.grantExecCap()
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	grant, err := auth.Verify(env, worker.issuerPub, time.Now().Unix(), nil)
	if err != nil {
		t.Fatalf("verify own cap: %v", err)
	}

	// A CID for bytes the worker's store does not hold.
	unknown, err := wasm.ComponentCID([]byte("not-the-hello-shard"))
	if err != nil {
		t.Fatal(err)
	}
	res, herr := worker.handleComputeSigned(ctx, contract.ComputeTask{TaskID: []byte("t"), Component: unknown.Bytes()}, grant)
	if herr != nil {
		t.Fatalf("handler error: %v", herr)
	}
	if res.OK {
		t.Fatal("worker executed an unresolvable CID")
	}
}

// --- The signed-cap gate: valid accepted, forged/tampered/wrong-issuer/expired
// rejected BEFORE any work runs. These exercise the mesh gate through the fabric,
// so a rejection means the worker never reached handleComputeSigned. ---

// dispatchOne is a helper: requester dispatches a task carrying caps[] and names
// issuer as the minting node, returning the worker's result.
func dispatchOne(t *testing.T, ctx context.Context, requester, worker *server, caps [][]byte, issuer contract.PeerID) contract.ComputeResult {
	t.Helper()
	c, err := requester.store.Put(HelloShardWASM())
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	task := contract.ComputeTask{TaskID: []byte("t"), Component: c.Bytes(), Caps: caps}
	res, err := requester.fabric.RequestComputeSigned(ctx, worker.fabric.PeerID(), task, issuer, 0)
	if err != nil {
		t.Fatalf("request compute: %v", err)
	}
	return res
}

func TestSignedCapForgedRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester := newTestNode(t, ctx, "requester")
	worker := newTestNode(t, ctx, "worker")
	requester.trustPeer(t, worker)
	if err := requester.fabric.Connect(ctx, worker.fabric.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// A cap minted by the REQUESTER (an attacker), naming the worker's issuer id so
	// it looks like the worker granted it. The worker resolves the worker's real
	// key; the signature (made by the requester's key) does not validate → rejected.
	res, env, err := requester.grantExecCap()
	_ = res
	if err != nil {
		t.Fatalf("attacker grant: %v", err)
	}
	out := dispatchOne(t, ctx, requester, worker, [][]byte{env}, worker.issuerID)
	if out.OK {
		t.Fatal("worker accepted a cap minted by an untrusted issuer (forged)")
	}
}

func TestSignedCapTamperedRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester := newTestNode(t, ctx, "requester")
	worker := newTestNode(t, ctx, "worker")
	requester.trustPeer(t, worker)
	if err := requester.fabric.Connect(ctx, worker.fabric.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	_, env, err := worker.grantExecCap()
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Flip a byte in the middle of the envelope (part of the signed preimage).
	tampered := append([]byte(nil), env...)
	tampered[len(tampered)/2] ^= 0xFF
	out := dispatchOne(t, ctx, requester, worker, [][]byte{tampered}, worker.issuerID)
	if out.OK {
		t.Fatal("worker accepted a tampered signed cap")
	}
}

func TestSignedCapWrongIssuerRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester := newTestNode(t, ctx, "requester")
	worker := newTestNode(t, ctx, "worker")
	stranger := newTestNode(t, ctx, "stranger")
	// Worker does NOT trust the stranger's key (never exchanged).
	requester.trustPeer(t, worker)
	if err := requester.fabric.Connect(ctx, worker.fabric.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// A perfectly valid cap, but minted by a stranger the worker never met.
	_, env, err := stranger.grantExecCap()
	if err != nil {
		t.Fatalf("stranger grant: %v", err)
	}
	out := dispatchOne(t, ctx, requester, worker, [][]byte{env}, stranger.issuerID)
	if out.OK {
		t.Fatal("worker accepted a cap from an issuer it never exchanged keys with")
	}
}

func TestSignedCapExpiredRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester := newTestNode(t, ctx, "requester")
	worker := newTestNode(t, ctx, "worker")
	requester.trustPeer(t, worker)
	if err := requester.fabric.Connect(ctx, worker.fabric.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Mint an exec cap that expired an hour ago on the worker's issuer key.
	res := contract.ResourceRef{Kind: contract.KindGPU, Path: "/cer/e2e/wasm/worker"}
	g, err := auth.NewGrant(res, []contract.Right{contract.RightExec}, nil, 0)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	g.NotBefore = time.Now().Add(-2 * time.Hour).Unix()
	g.Expiry = time.Now().Add(-time.Hour).Unix()
	env, err := worker.signer.Issue(g)
	if err != nil {
		t.Fatalf("issue expired: %v", err)
	}
	out := dispatchOne(t, ctx, requester, worker, [][]byte{env}, worker.issuerID)
	if out.OK {
		t.Fatal("worker accepted an expired signed cap")
	}
}

func TestSignedCapMissingRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester := newTestNode(t, ctx, "requester")
	worker := newTestNode(t, ctx, "worker")
	requester.trustPeer(t, worker)
	if err := requester.fabric.Connect(ctx, worker.fabric.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// No cap at all in the task.
	out := dispatchOne(t, ctx, requester, worker, nil, worker.issuerID)
	if out.OK {
		t.Fatal("worker ran a task with no signed capability")
	}
}
