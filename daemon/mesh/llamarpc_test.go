package mesh

// llamarpc_test.go covers the signed-capability gate on the cross-node llama.cpp
// offload session (ServeLlamaRPC / OpenLlamaRPCSession / verifyLlamaRPCCap).
//
// These tests need NO model weights and NO llama.cpp binaries: the backend is a
// tiny in-test echo server, so the whole matrix runs on every OS leg in CI. That
// is deliberate. The reason the mock inference rotted undetected for so long is
// that its only test skipped itself unless CERBERUS_LLAMA_MODEL was set, and CI
// never set it. A gate whose tests skip is a gate nobody is checking.
//
// The property under test is the one that matters for CVE-2026-34159: an
// unauthorized peer must never reach the backend AT ALL. Not "reaches it and is
// rejected" — never reaches it, because reaching it means bytes hit a C++
// deserializer that trusts its peer. Every denial case therefore asserts
// opened() == false, not merely that an error came back.

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
)

const testBuild = 10021

// echoLlamaBackend stands in for daemon/llama's real worker. OpenLocal hands back
// a pipe that echoes; it records whether it was EVER invoked so a denial test can
// prove the gate ran first.
type echoLlamaBackend struct {
	mu       sync.Mutex
	opened   bool
	build    int
	failWith error
	conns    []*echoConn
}

func newEchoLlamaBackend() *echoLlamaBackend {
	return &echoLlamaBackend{build: testBuild}
}

func (e *echoLlamaBackend) Build() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.build == 0 {
		return testBuild
	}
	return e.build
}

func (e *echoLlamaBackend) OpenLocal(context.Context, auth.Grant) (io.ReadWriteCloser, error) {
	e.mu.Lock()
	e.opened = true
	failWith := e.failWith
	e.mu.Unlock()
	if failWith != nil {
		return nil, failWith
	}
	c := newEchoConn()
	e.mu.Lock()
	e.conns = append(e.conns, c)
	e.mu.Unlock()
	return c, nil
}

func (e *echoLlamaBackend) wasOpened() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.opened
}

// echoConn is a ReadWriteCloser that echoes everything written to it back out,
// standing in for a ggml-rpc-server on loopback.
type echoConn struct {
	pr *io.PipeReader
	pw *io.PipeWriter
}

func newEchoConn() *echoConn {
	pr, pw := io.Pipe()
	return &echoConn{pr: pr, pw: pw}
}

func (c *echoConn) Read(p []byte) (int, error)  { return c.pr.Read(p) }
func (c *echoConn) Write(p []byte) (int, error) { return c.pw.Write(p) }
func (c *echoConn) Close() error {
	_ = c.pw.Close()
	return c.pr.Close()
}

// llamaCapAs mints a signed offload capability naming LlamaRPCResource(site) with
// the given right, self-issued under requester's own mesh identity (the
// SelfIssuerResolver trust model).
func llamaCapAs(t *testing.T, requester *Fabric, site string, right contract.Right, ttl time.Duration) []byte {
	t.Helper()
	ks, err := auth.NewMemoryKeyStore(requester.Identity().Seed())
	if err != nil {
		t.Fatalf("keystore from fabric identity: %v", err)
	}
	sc := auth.NewSignedCap(ks)
	g, err := auth.NewGrant(LlamaRPCResource(site), []contract.Right{right}, nil, ttl)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	env, err := sc.Issue(g)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return env
}

func twoLlamaNodes(t *testing.T, ctx context.Context) (requester, server *Fabric) {
	t.Helper()
	requester, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	t.Cleanup(func() { _ = requester.Close() })
	server, err = New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := requester.Connect(ctx, server.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return requester, server
}

func nowFn() int64 { return time.Now().Unix() }

// assertBackendUntouched is the assertion every denial case shares.
func assertBackendUntouched(t *testing.T, be *echoLlamaBackend, what string) {
	t.Helper()
	// Give an erroneously-started backend a beat to show itself.
	time.Sleep(200 * time.Millisecond)
	if be.wasOpened() {
		t.Fatalf("%s: OpenLocal was invoked despite a failed gate — no ggml-rpc-server may be "+
			"spawned and no socket dialed before the capability verifies (CVE-2026-34159 makes this "+
			"the difference between a refused session and a remote code execution surface)", what)
	}
}

// TestLlamaRPCSessionWithValidCap is the happy path, and it proves the property
// the whole lane rests on: after a valid RightExec cap opens the session, bytes
// travel requester -> real mesh QUIC stream -> serving node's backend and back,
// UNMODIFIED and with no framing imposed on top.
//
// That last part is why the assertion is a byte-for-byte round trip rather than
// "the backend was called". ggml-rpc does its own framing; if Cerberus added a
// length prefix, or stranded bytes in a buffer, or dropped a direction, this test
// is what notices. The payload deliberately includes bytes that would look like a
// length header to a framing layer.
func TestLlamaRPCSessionWithValidCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoLlamaNodes(t, ctx)

	// The serving node's backend echoes: whatever the peer sends comes straight
	// back, standing in for a ggml-rpc-server on loopback.
	be := newEchoLlamaBackend()
	server.ServeLlamaRPC(be, SelfIssuerResolver, nowFn, nil)

	env := llamaCapAs(t, requester, "test", contract.RightExec, time.Hour)

	// Bytes that a naive framing layer would misread as a 4-byte big-endian length.
	payload := []byte{0x00, 0x00, 0x00, 0x08, 'g', 'g', 'm', 'l', 0xFF, 0x00, 0x01}

	sendR, sendW := io.Pipe() // test -> local.Read -> mesh -> peer
	recvR, recvW := io.Pipe() // peer -> mesh -> local.Write -> test
	local := &pipeRW{r: sendR, w: recvW}

	sessionErr := make(chan error, 1)
	go func() {
		sessionErr <- requester.OpenLlamaRPCSession(server.PeerID(), env, requester.PeerID(), testBuild, local)
	}()

	go func() {
		_, _ = sendW.Write(payload)
	}()

	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, len(payload))
		n, err := io.ReadFull(recvR, buf)
		if err != nil && n == 0 {
			return
		}
		got <- buf[:n]
	}()

	select {
	case b := <-got:
		if !bytes.Equal(b, payload) {
			t.Fatalf("round-trip corrupted the stream:\n sent %x\n got  %x", payload, b)
		}
	case err := <-sessionErr:
		t.Fatalf("session ended before bytes round-tripped: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for bytes to round-trip through the capability-gated session")
	}

	if !be.wasOpened() {
		t.Fatal("bytes round-tripped without the backend ever opening — the test is not proving what it claims")
	}

	_ = sendW.Close()
	_ = recvW.Close()
}

// TestLlamaRPCDeniedWithNoCapability — a session with no envelope at all.
func TestLlamaRPCDeniedWithNoCapability(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoLlamaNodes(t, ctx)

	be := newEchoLlamaBackend()
	server.ServeLlamaRPC(be, SelfIssuerResolver, nowFn, nil)

	var buf bytes.Buffer
	err := requester.OpenLlamaRPCSession(server.PeerID(), nil, contract.PeerID{}, testBuild, &buf)
	if err == nil {
		t.Fatal("offload session with no capability envelope was accepted")
	}
	assertBackendUntouched(t, be, "missing capability")
}

// TestLlamaRPCDeniedWithForgedCap — a syntactically plausible envelope that was
// never signed by the issuer's key.
func TestLlamaRPCDeniedWithForgedCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoLlamaNodes(t, ctx)

	be := newEchoLlamaBackend()
	server.ServeLlamaRPC(be, SelfIssuerResolver, nowFn, nil)

	// Take a genuine envelope and corrupt its signature bytes.
	env := llamaCapAs(t, requester, "test", contract.RightExec, time.Hour)
	forged := append([]byte(nil), env...)
	forged[len(forged)-1] ^= 0xFF
	forged[len(forged)-2] ^= 0xFF

	var buf bytes.Buffer
	if err := requester.OpenLlamaRPCSession(server.PeerID(), forged, requester.PeerID(), testBuild, &buf); err == nil {
		t.Fatal("offload session with a forged capability was accepted")
	}
	assertBackendUntouched(t, be, "forged capability")
}

// TestLlamaRPCDeniedWithExpiredCap — a validly-signed cap outside its window.
func TestLlamaRPCDeniedWithExpiredCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoLlamaNodes(t, ctx)

	be := newEchoLlamaBackend()
	// Serve with a clock far in the future so a 1s-TTL cap is long expired.
	future := func() int64 { return time.Now().Add(24 * time.Hour).Unix() }
	server.ServeLlamaRPC(be, SelfIssuerResolver, future, nil)

	env := llamaCapAs(t, requester, "test", contract.RightExec, time.Second)

	var buf bytes.Buffer
	if err := requester.OpenLlamaRPCSession(server.PeerID(), env, requester.PeerID(), testBuild, &buf); err == nil {
		t.Fatal("offload session with an expired capability was accepted")
	}
	assertBackendUntouched(t, be, "expired capability")
}

// TestLlamaRPCDeniedWithRevokedCap — a valid, unexpired cap whose id is revoked.
// This is the one that matters operationally: it is how an operator cuts off a
// peer they have already trusted.
func TestLlamaRPCDeniedWithRevokedCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoLlamaNodes(t, ctx)

	be := newEchoLlamaBackend()
	revokeAll := func(contract.CapID) bool { return true }
	server.ServeLlamaRPC(be, SelfIssuerResolver, nowFn, revokeAll)

	env := llamaCapAs(t, requester, "test", contract.RightExec, time.Hour)

	var buf bytes.Buffer
	if err := requester.OpenLlamaRPCSession(server.PeerID(), env, requester.PeerID(), testBuild, &buf); err == nil {
		t.Fatal("offload session with a revoked capability was accepted")
	}
	assertBackendUntouched(t, be, "revoked capability")
}

// TestLlamaRPCDeniedWithWrongRight — RightRead does not authorize execution.
// Given CVE-2026-34159, the distinction between "may read" and "may exec" on this
// resource is the distinction between a file and a shell.
func TestLlamaRPCDeniedWithWrongRight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoLlamaNodes(t, ctx)

	be := newEchoLlamaBackend()
	server.ServeLlamaRPC(be, SelfIssuerResolver, nowFn, nil)

	env := llamaCapAs(t, requester, "test", contract.RightRead, time.Hour) // not RightExec

	var buf bytes.Buffer
	if err := requester.OpenLlamaRPCSession(server.PeerID(), env, requester.PeerID(), testBuild, &buf); err == nil {
		t.Fatal("offload session with only a RightRead cap was accepted (must require RightExec)")
	}
	assertBackendUntouched(t, be, "wrong right")
}

// TestLlamaRPCDeniedWithWrongIssuer — a cap minted under a third identity, whose
// claimed issuer is not the peer the QUIC/TLS handshake authenticated for this
// stream. The self-issuer binding rejects it before auth.Verify even runs.
func TestLlamaRPCDeniedWithWrongIssuer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoLlamaNodes(t, ctx)

	other, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("other: %v", err)
	}
	defer other.Close()

	be := newEchoLlamaBackend()
	server.ServeLlamaRPC(be, SelfIssuerResolver, nowFn, nil)

	// Genuinely signed — by the wrong key, for the wrong peer.
	env := llamaCapAs(t, other, "test", contract.RightExec, time.Hour)

	var buf bytes.Buffer
	if err := requester.OpenLlamaRPCSession(server.PeerID(), env, other.PeerID(), testBuild, &buf); err == nil {
		t.Fatal("offload session with a wrong-issuer cap was accepted")
	}
	assertBackendUntouched(t, be, "wrong issuer")
}

// TestLlamaRPCDeniedWhenWorkerDisabled — worker mode ships OFF. A nil backend must
// refuse cleanly and say how to enable it, not panic.
func TestLlamaRPCDeniedWhenWorkerDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoLlamaNodes(t, ctx)

	server.ServeLlamaRPC(nil, SelfIssuerResolver, nowFn, nil)

	env := llamaCapAs(t, requester, "test", contract.RightExec, time.Hour)
	var buf bytes.Buffer
	err := requester.OpenLlamaRPCSession(server.PeerID(), env, requester.PeerID(), testBuild, &buf)
	if err == nil {
		t.Fatal("offload session was accepted on a node with no llama worker configured")
	}
}

// TestLlamaRPCRefusesBuildSkew — mismatched llama.cpp builds must be refused
// LOUDLY at the gate rather than letting a C++ deserializer meet bytes from a
// build it does not agree with. ggml-rpc's wire format is an internal protocol
// between matched binaries; a mismatch misparses rather than failing cleanly.
func TestLlamaRPCRefusesBuildSkew(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoLlamaNodes(t, ctx)

	be := newEchoLlamaBackend()
	be.build = testBuild
	server.ServeLlamaRPC(be, SelfIssuerResolver, nowFn, nil)

	env := llamaCapAs(t, requester, "test", contract.RightExec, time.Hour)

	var buf bytes.Buffer
	err := requester.OpenLlamaRPCSession(server.PeerID(), env, requester.PeerID(), testBuild+1, &buf)
	if err == nil {
		t.Fatal("offload session across mismatched llama.cpp builds was accepted")
	}
	// Skew is refused AFTER the cap verifies but BEFORE the worker starts: there is
	// no point spawning a process for a session that cannot work.
	assertBackendUntouched(t, be, "build skew")
}

// capForResourceAs mints a signed cap for an ARBITRARY resource, self-issued
// under requester's identity. Used to prove resource scoping.
func capForResourceAs(t *testing.T, requester *Fabric, res contract.ResourceRef, right contract.Right, ttl time.Duration) []byte {
	t.Helper()
	ks, err := auth.NewMemoryKeyStore(requester.Identity().Seed())
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	g, err := auth.NewGrant(res, []contract.Right{right}, nil, ttl)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	env, err := auth.NewSignedCap(ks).Issue(g)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return env
}

// TestLlamaRPCDeniedWithCapForDifferentResource is the test that makes the word
// "capability" honest on this path.
//
// The cap here is entirely legitimate: correctly signed, self-issued by the
// authenticated peer, unexpired, unrevoked, and carrying RightExec. It is simply
// scoped to a DIFFERENT resource — MeshComputeResource, i.e. permission to run a
// WASM workload, not to run llama.cpp tensor work.
//
// Every other gate in this package USED to accept exactly this, because none of
// them compared grant.Resource to what was being accessed (auth.Verify does not
// even take the resource as an argument). They all scope now — see capscope.go and
// the sibling *DeniedWithCapForDifferentResource tests — but the stakes are
// highest here: accepting it would mean a peer trusted to run a sandboxed WASM job
// could instead open a ggml-rpc session, a CVE-2026-34159 pre-auth RCE surface,
// using a capability that never mentioned llama.
//
// If this test starts failing, the gate has regressed to a signed permission slip.
func TestLlamaRPCDeniedWithCapForDifferentResource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, server := twoLlamaNodes(t, ctx)

	be := newEchoLlamaBackend()
	server.ServeLlamaRPC(be, SelfIssuerResolver, nowFn, nil)

	// A REAL, valid, RightExec capability — for the compute resource, not llama.
	env := capForResourceAs(t, requester, MeshComputeResource("test"), contract.RightExec, time.Hour)

	var buf bytes.Buffer
	err := requester.OpenLlamaRPCSession(server.PeerID(), env, requester.PeerID(), testBuild, &buf)
	if err == nil {
		t.Fatal("a RightExec capability minted for MeshComputeResource opened a LLAMA offload " +
			"session — the gate is not resource-scoped, so any RightExec cap grants ggml-rpc access")
	}
	assertBackendUntouched(t, be, "capability for a different resource")
}

// TestGrantCoversLlamaResource unit-tests the scoping predicate directly, so the
// boundary is pinned without needing a live mesh. The predicate is now the shared
// grantCoversResource in capscope.go, which every gate in this package uses; this
// test keeps pinning it from llama's point of view.
func TestGrantCoversLlamaResource(t *testing.T) {
	want := LlamaRPCResource("site-a")

	if err := grantCoversResource("llama rpc", auth.Grant{Resource: want}, want); err != nil {
		t.Fatalf("exact resource match rejected: %v", err)
	}
	// Right kind, wrong path (another site's worker).
	if err := grantCoversResource("llama rpc", auth.Grant{Resource: LlamaRPCResource("site-b")}, want); err == nil {
		t.Fatal("a cap for another site's llama resource was accepted")
	}
	// Right path, wrong kind.
	if err := grantCoversResource("llama rpc", auth.Grant{Resource: contract.ResourceRef{Kind: contract.KindFS, Path: want.Path}}, want); err == nil {
		t.Fatal("a cap of the wrong Kind was accepted")
	}
	// The realistic attack: a valid mesh-compute cap.
	if err := grantCoversResource("llama rpc", auth.Grant{Resource: MeshComputeResource("site-a")}, want); err == nil {
		t.Fatal("a MeshComputeResource cap was accepted for llama offload")
	}
	// Zero grant.
	if err := grantCoversResource("llama rpc", auth.Grant{}, want); err == nil {
		t.Fatal("a zero-resource grant was accepted")
	}
}

// TestLlamaRPCResourceNamesGPUKind pins the resource convention. KindGPU is reused
// deliberately: minting a new ResourceKind would be a frozen-contract change
// (schemas/capability.cddl mirrors the Go enum), and KindGPU already means "this
// node's best available compute backend" in daemon/gpu, which is exactly what a
// ggml-rpc worker offers.
func TestLlamaRPCResourceNamesGPUKind(t *testing.T) {
	r := LlamaRPCResource("site-a")
	if r.Kind != contract.KindGPU {
		t.Fatalf("Kind = %q, want %q", r.Kind, contract.KindGPU)
	}
	if r.Path != "cerberus/site-a/llama-rpc" {
		t.Fatalf("Path = %q", r.Path)
	}
}

// pipeRW adapts a reader/writer pair to io.ReadWriter for the session client.
type pipeRW struct {
	r io.Reader
	w io.Writer
}

func (p *pipeRW) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeRW) Write(b []byte) (int, error) { return p.w.Write(b) }
