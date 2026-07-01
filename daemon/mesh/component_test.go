package mesh

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/wasm"
	"github.com/ipfs/go-cid"
)

// newComponentFetchCap mints a signed capability naming mesh.MeshFabricResource
// (site "test") with RightRead — the credential ServeComponentFetch's gate
// requires — under a fresh in-memory issuer key, returning the envelope, the
// issuer's PeerID, and its public key (for building a resolver).
func newComponentFetchCap(t *testing.T, site string) (env []byte, issuerID contract.PeerID, issuerPub ed25519.PublicKey) {
	t.Helper()
	sc, pub := newTestSignedCap(t)
	copy(issuerID[:], pub)
	g, err := auth.NewGrant(MeshFabricResource(site), []contract.Right{contract.RightRead}, nil, time.Hour)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	e, err := sc.Issue(g)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return e, issuerID, pub
}

// singleIssuerResolver returns an IssuerPubResolver that trusts exactly id ->
// pub, mirroring what a real discovery-exchanged trust store would resolve for
// a single known peer.
func singleIssuerResolver(id contract.PeerID, pub ed25519.PublicKey) IssuerPubResolver {
	return func(claimed contract.PeerID) (ed25519.PublicKey, bool) {
		if claimed == id {
			return pub, true
		}
		return nil, false
	}
}

// TestRequestComponentOverMesh proves the real p2p fetch path end-to-end: node A
// has a component registered locally (in its ContentStore); node B does not.
// B asks A for the bytes behind the CID over the mesh, presenting a valid
// mesh-fabric-membership capability, and must receive back bytes that hash to
// exactly the CID it asked for.
func TestRequestComponentOverMesh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node a: %v", err)
	}
	defer a.Close()
	b, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node b: %v", err)
	}
	defer b.Close()
	if err := b.Connect(ctx, a.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Only A has the component.
	aStore := wasm.NewContentStore()
	component := []byte{0x00, 0x61, 0x73, 0x6d, 1, 2, 3, 4, 5}
	c, err := aStore.Put(component)
	if err != nil {
		t.Fatalf("seed a's store: %v", err)
	}

	env, issuerID, issuerPub := newComponentFetchCap(t, "test")
	a.ServeComponentFetch(aStore, singleIssuerResolver(issuerID, issuerPub), func() int64 { return time.Now().Unix() }, nil)

	bStore := wasm.NewContentStore()
	if bStore.Has(c) {
		t.Fatal("test setup bug: b already has the component")
	}

	got, err := b.RequestComponent(ctx, a.PeerID(), c, env, issuerID)
	if err != nil {
		t.Fatalf("b failed to fetch component from a: %v", err)
	}
	gotCID, err := wasm.ComponentCID(got)
	if err != nil {
		t.Fatalf("hash fetched bytes: %v", err)
	}
	if !gotCID.Equals(c) {
		t.Fatalf("fetched bytes hash to %s, want %s", gotCID, c)
	}

	// Populate b's own store, proving a subsequent local lookup now hits.
	if _, err := bStore.Put(got); err != nil {
		t.Fatalf("populate b's store: %v", err)
	}
	if !bStore.Has(c) {
		t.Fatal("b's store was not populated by the fetched bytes")
	}
}

// TestRequestComponentRejectsTamperedBytes proves the requester-side integrity
// check: if a peer's response carries bytes that do not hash to the requested
// CID, RequestComponent must reject them rather than handing them back to the
// caller. We simulate a tampering peer by wiring a fake stream handler that
// returns the wrong bytes for the requested CID.
func TestRequestComponentRejectsTamperedBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node a: %v", err)
	}
	defer a.Close()
	b, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node b: %v", err)
	}
	defer b.Close()
	if err := b.Connect(ctx, a.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	wanted := []byte("the-real-component-bytes")
	wantedCID, err := wasm.ComponentCID(wanted)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	env, issuerID, issuerPub := newComponentFetchCap(t, "test")
	// A malicious/buggy responder: register a store that reports Has=true for
	// anything but Get returns different bytes than what was asked for.
	a.ServeComponentFetch(tamperingSource{actualBytes: []byte("substituted-bytes")}, singleIssuerResolver(issuerID, issuerPub), func() int64 { return time.Now().Unix() }, nil)

	if _, err := b.RequestComponent(ctx, a.PeerID(), wantedCID, env, issuerID); err == nil {
		t.Fatal("RequestComponent accepted bytes that do not hash to the requested CID")
	}
}

// TestRequestComponentMissingFailsCleanly proves the failure path: asking a
// peer for a CID it genuinely does not have (but with a VALID capability)
// returns a clear error, not a hang or a panic.
func TestRequestComponentMissingFailsCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node a: %v", err)
	}
	defer a.Close()
	b, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node b: %v", err)
	}
	defer b.Close()
	if err := b.Connect(ctx, a.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	env, issuerID, issuerPub := newComponentFetchCap(t, "test")
	// A's store is empty — it genuinely has nothing.
	a.ServeComponentFetch(wasm.NewContentStore(), singleIssuerResolver(issuerID, issuerPub), func() int64 { return time.Now().Unix() }, nil)

	absentCID, err := wasm.ComponentCID([]byte("nobody-has-this"))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	done := make(chan struct{})
	var fetchErr error
	go func() {
		defer close(done)
		_, fetchErr = b.RequestComponent(ctx, a.PeerID(), absentCID, env, issuerID)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RequestComponent for a missing CID hung instead of returning an error")
	}
	if fetchErr == nil {
		t.Fatal("RequestComponent for a CID no peer has must fail, not succeed")
	}
}

// TestComponentFetchDeniedWithNoCapability proves the capability gate rejects a
// request with NO envelope at all, BEFORE the local ComponentSource is ever
// consulted — even though A genuinely holds the component. This is the
// concrete "no check at all" regression test: an empty Cap must not fall back
// to "public read", it must fail closed.
func TestComponentFetchDeniedWithNoCapability(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node a: %v", err)
	}
	defer a.Close()
	b, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node b: %v", err)
	}
	defer b.Close()
	if err := b.Connect(ctx, a.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	aStore := wasm.NewContentStore()
	component := []byte{0x00, 0x61, 0x73, 0x6d, 9, 9, 9}
	c, err := aStore.Put(component)
	if err != nil {
		t.Fatalf("seed a's store: %v", err)
	}
	fetchCalled := false
	trackingStore := trackingComponentSource{ComponentSource: aStore, called: &fetchCalled}
	_, _, issuerPub := newComponentFetchCap(t, "test")
	var issuerID contract.PeerID
	copy(issuerID[:], issuerPub)
	a.ServeComponentFetch(trackingStore, singleIssuerResolver(issuerID, issuerPub), func() int64 { return time.Now().Unix() }, nil)

	// No envelope at all.
	if _, err := b.RequestComponent(ctx, a.PeerID(), c, nil, contract.PeerID{}); err == nil {
		t.Fatal("RequestComponent with no capability envelope was accepted")
	}
	if fetchCalled {
		t.Fatal("local ComponentSource was consulted despite a missing capability — gate is not fail-closed")
	}
}

// TestComponentFetchDeniedWithForgedIssuer proves a syntactically valid,
// correctly self-signed envelope is still denied when the peer's resolver does
// not recognize the claimed issuer (never exchanged at discovery) — a stranger
// cannot mint its own key and have it trusted.
func TestComponentFetchDeniedWithForgedIssuer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node a: %v", err)
	}
	defer a.Close()
	b, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node b: %v", err)
	}
	defer b.Close()
	if err := b.Connect(ctx, a.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	aStore := wasm.NewContentStore()
	component := []byte{0x00, 0x61, 0x73, 0x6d, 1, 1, 1}
	c, err := aStore.Put(component)
	if err != nil {
		t.Fatalf("seed a's store: %v", err)
	}

	// A's resolver trusts nobody (empty trust store) — mirroring a real node
	// that never exchanged keys with the attacker at discovery.
	noResolver := func(contract.PeerID) (ed25519.PublicKey, bool) { return nil, false }
	a.ServeComponentFetch(aStore, noResolver, func() int64 { return time.Now().Unix() }, nil)

	// A stranger mints its own, perfectly validly-signed cap.
	env, issuerID, _ := newComponentFetchCap(t, "test")
	if _, err := b.RequestComponent(ctx, a.PeerID(), c, env, issuerID); err == nil {
		t.Fatal("RequestComponent accepted a capability from an issuer the peer never trusted")
	}
}

// TestComponentFetchDeniedWithExpiredCapability proves an otherwise-valid
// envelope that has expired is denied.
func TestComponentFetchDeniedWithExpiredCapability(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node a: %v", err)
	}
	defer a.Close()
	b, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node b: %v", err)
	}
	defer b.Close()
	if err := b.Connect(ctx, a.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	aStore := wasm.NewContentStore()
	component := []byte{0x00, 0x61, 0x73, 0x6d, 2, 2, 2}
	c, err := aStore.Put(component)
	if err != nil {
		t.Fatalf("seed a's store: %v", err)
	}

	sc, pub := newTestSignedCap(t)
	var issuerID contract.PeerID
	copy(issuerID[:], pub)
	a.ServeComponentFetch(aStore, singleIssuerResolver(issuerID, pub), func() int64 { return time.Now().Unix() }, nil)

	g, err := auth.NewGrant(MeshFabricResource("test"), []contract.Right{contract.RightRead}, nil, 0)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	g.NotBefore = time.Now().Add(-2 * time.Hour).Unix()
	g.Expiry = time.Now().Add(-time.Hour).Unix()
	env, err := sc.Issue(g)
	if err != nil {
		t.Fatalf("issue expired: %v", err)
	}

	if _, err := b.RequestComponent(ctx, a.PeerID(), c, env, issuerID); err == nil {
		t.Fatal("RequestComponent accepted an expired capability")
	}
}

// TestComponentFetchDeniedWithRevokedCapability proves a capability that
// verifies cryptographically but whose id the peer's revocation predicate
// flags is denied.
func TestComponentFetchDeniedWithRevokedCapability(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node a: %v", err)
	}
	defer a.Close()
	b, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node b: %v", err)
	}
	defer b.Close()
	if err := b.Connect(ctx, a.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	aStore := wasm.NewContentStore()
	component := []byte{0x00, 0x61, 0x73, 0x6d, 3, 3, 3}
	c, err := aStore.Put(component)
	if err != nil {
		t.Fatalf("seed a's store: %v", err)
	}

	sc, pub := newTestSignedCap(t)
	var issuerID contract.PeerID
	copy(issuerID[:], pub)

	g, err := auth.NewGrant(MeshFabricResource("test"), []contract.Right{contract.RightRead}, nil, time.Hour)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	env, err := sc.Issue(g)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	verifiedOnce, err := auth.Verify(env, pub, time.Now().Unix(), nil)
	if err != nil {
		t.Fatalf("pre-check verify: %v", err)
	}
	isRevoked := func(id contract.CapID) bool { return id == verifiedOnce.ID }

	a.ServeComponentFetch(aStore, singleIssuerResolver(issuerID, pub), func() int64 { return time.Now().Unix() }, isRevoked)

	if _, err := b.RequestComponent(ctx, a.PeerID(), c, env, issuerID); err == nil {
		t.Fatal("RequestComponent accepted a revoked capability")
	}
}

// TestComponentFetchDeniedWithWrongRight proves a capability that lacks
// RightRead (e.g. only conveys RightWrite) is denied.
func TestComponentFetchDeniedWithWrongRight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node a: %v", err)
	}
	defer a.Close()
	b, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node b: %v", err)
	}
	defer b.Close()
	if err := b.Connect(ctx, a.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	aStore := wasm.NewContentStore()
	component := []byte{0x00, 0x61, 0x73, 0x6d, 4, 4, 4}
	c, err := aStore.Put(component)
	if err != nil {
		t.Fatalf("seed a's store: %v", err)
	}

	sc, pub := newTestSignedCap(t)
	var issuerID contract.PeerID
	copy(issuerID[:], pub)
	a.ServeComponentFetch(aStore, singleIssuerResolver(issuerID, pub), func() int64 { return time.Now().Unix() }, nil)

	g, err := auth.NewGrant(MeshFabricResource("test"), []contract.Right{contract.RightWrite}, nil, time.Hour)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	env, err := sc.Issue(g)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if _, err := b.RequestComponent(ctx, a.PeerID(), c, env, issuerID); err == nil {
		t.Fatal("RequestComponent accepted a capability lacking RightRead")
	}
}

// trackingComponentSource wraps a ComponentSource and records whether Has/Get
// was ever called, so a denial test can prove the local store was never
// consulted at all.
type trackingComponentSource struct {
	ComponentSource
	called *bool
}

func (t trackingComponentSource) Has(c cid.Cid) bool {
	*t.called = true
	return t.ComponentSource.Has(c)
}

func (t trackingComponentSource) Get(c cid.Cid) ([]byte, error) {
	*t.called = true
	return t.ComponentSource.Get(c)
}

// tamperingSource implements ComponentSource but always returns actualBytes
// regardless of the CID asked for — simulating a peer that (maliciously or by
// bug) substitutes different bytes for a requested CID, so we can prove
// RequestComponent's integrity check rejects it.
type tamperingSource struct {
	actualBytes []byte
}

func (t tamperingSource) Has(c cid.Cid) bool { return true }
func (t tamperingSource) Get(c cid.Cid) ([]byte, error) {
	return t.actualBytes, nil
}
