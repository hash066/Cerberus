package mesh

// shard_test.go covers the signed-capability gate on the shard put/get RPC
// (ServeShards/RequestPutShard/RequestGetShard/verifyShardCap/shardRightFor),
// which shipped with NO capability check at all before this change (see
// shard.go's header comment). These tests prove: (a) a valid RightWrite/
// RightRead capability lets a put/get through end-to-end over the real mesh;
// (b) a request with no envelope is denied BEFORE any local store mutation or
// read; (c) a forged/wrong-issuer/expired/revoked/wrong-right envelope is
// denied the same way, fail-closed.

import (
	"context"
	"errors"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

// trackingShardServer wraps a ShardServer and records whether PutShard/GetShard
// was ever invoked, so a denial test can prove the local store was never
// touched at all.
type trackingShardServer struct {
	ShardServer
	putCalled *bool
	getCalled *bool
}

func (t trackingShardServer) PutShard(cidBytes []byte, shard []byte) error {
	*t.putCalled = true
	return t.ShardServer.PutShard(cidBytes, shard)
}

func (t trackingShardServer) GetShard(cidBytes []byte) ([]byte, error) {
	*t.getCalled = true
	return t.ShardServer.GetShard(cidBytes)
}

// memShardServer is a minimal in-memory ShardServer for tests.
type memShardServer struct {
	data map[string][]byte
}

func newMemShardServer() *memShardServer { return &memShardServer{data: map[string][]byte{}} }

func (m *memShardServer) PutShard(cidBytes []byte, shard []byte) error {
	m.data[string(cidBytes)] = append([]byte(nil), shard...)
	return nil
}

func (m *memShardServer) GetShard(cidBytes []byte) ([]byte, error) {
	b, ok := m.data[string(cidBytes)]
	if !ok {
		return nil, errShardTestNotFound
	}
	return b, nil
}

var errShardTestNotFound = errors.New("mesh: test shard not found")

// dummyCID builds a stable content-address for a small byte slice, for tests
// that only need a syntactically valid CID (ShardServer's PutShard/GetShard
// take raw CID bytes and never re-verify shard content hashing themselves —
// that discipline lives in daemon/dfs).
func dummyCID(t *testing.T, seed byte) cid.Cid {
	t.Helper()
	mh, err := multihash.Sum([]byte{seed, seed, seed}, multihash.SHA2_256, -1)
	if err != nil {
		t.Fatalf("build cid: %v", err)
	}
	return cid.NewCidV1(cid.Raw, mh)
}

// shardSignerForFabric builds an *auth.SignedCap issuer keyed off f's OWN mesh
// identity (f.Identity()), exactly as daemon/system.NewShardCapSigner does —
// so a cap it mints names f.PeerID() as issuer, matching the PeerID the
// QUIC/TLS handshake authenticates when f dials out. This is what makes a
// "valid" self-issued shard cap actually pass ServeShards's stream-binding
// check in these tests.
func shardSignerForFabric(t *testing.T, f *Fabric) *auth.SignedCap {
	t.Helper()
	ks, err := auth.NewMemoryKeyStore(f.Identity().Seed())
	if err != nil {
		t.Fatalf("keystore from fabric identity: %v", err)
	}
	return auth.NewSignedCap(ks)
}

// mintShardCapAs mints a signed capability naming MeshShardResource(site) with
// the given right, self-issued under requester's own mesh identity so its
// issuer equals requester.PeerID() — the identity the worker's stream will
// authenticate when requester dials in.
func mintShardCapAs(t *testing.T, requester *Fabric, site string, right contract.Right, ttl time.Duration) []byte {
	t.Helper()
	sc := shardSignerForFabric(t, requester)
	g, err := auth.NewGrant(MeshShardResource(site), []contract.Right{right}, nil, ttl)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	env, err := sc.Issue(g)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return env
}

// TestShardPutGetRoundTripWithValidCap proves the happy path end-to-end over
// the real mesh transport: a requester self-issues a RightWrite capability,
// puts a shard on a peer, then self-issues a RightRead capability and reads it
// back — genuinely crossing the network to the peer's own local store.
func TestShardPutGetRoundTripWithValidCap(t *testing.T) {
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

	local := newMemShardServer()
	worker.ServeShards(local, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	c := dummyCID(t, 0x11)
	shardBytes := []byte("real-shard-bytes-cross-the-network")

	putEnv := mintShardCapAs(t, requester, "test", contract.RightWrite, time.Hour)
	if err := requester.RequestPutShard(ctx, worker.PeerID(), c.Bytes(), shardBytes, putEnv, requester.PeerID()); err != nil {
		t.Fatalf("put shard with valid cap denied: %v", err)
	}

	getEnv := mintShardCapAs(t, requester, "test", contract.RightRead, time.Hour)
	got, err := requester.RequestGetShard(ctx, worker.PeerID(), c.Bytes(), getEnv, requester.PeerID())
	if err != nil {
		t.Fatalf("get shard with valid cap denied: %v", err)
	}
	if string(got) != string(shardBytes) {
		t.Fatalf("got %q, want %q", got, shardBytes)
	}
}

// TestShardPutDeniedWithNoCapability proves a put request carrying NO envelope
// is denied BEFORE local.PutShard is ever called — fail closed, not "write
// anyway".
func TestShardPutDeniedWithNoCapability(t *testing.T) {
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

	var putCalled, getCalled bool
	local := trackingShardServer{ShardServer: newMemShardServer(), putCalled: &putCalled, getCalled: &getCalled}
	worker.ServeShards(local, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	c := dummyCID(t, 0x22)
	if err := requester.RequestPutShard(ctx, worker.PeerID(), c.Bytes(), []byte("data"), nil, contract.PeerID{}); err == nil {
		t.Fatal("put shard with no capability envelope was accepted")
	}
	if putCalled {
		t.Fatal("local.PutShard was invoked despite a missing capability — gate is not fail-closed")
	}
}

// TestShardGetDeniedWithNoCapability proves a get request carrying NO envelope
// is denied BEFORE local.GetShard is ever called, and returns no data — even
// though the shard genuinely exists in the peer's store.
func TestShardGetDeniedWithNoCapability(t *testing.T) {
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

	var putCalled, getCalled bool
	local := newMemShardServer()
	c := dummyCID(t, 0x33)
	_ = local.PutShard(c.Bytes(), []byte("pre-existing-secret-shard"))
	tracked := trackingShardServer{ShardServer: local, putCalled: &putCalled, getCalled: &getCalled}
	worker.ServeShards(tracked, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	data, err := requester.RequestGetShard(ctx, worker.PeerID(), c.Bytes(), nil, contract.PeerID{})
	if err == nil {
		t.Fatal("get shard with no capability envelope was accepted")
	}
	if len(data) != 0 {
		t.Fatal("get shard with no capability returned data")
	}
	if getCalled {
		t.Fatal("local.GetShard was invoked despite a missing capability — a shard that exists must not be readable without a cap")
	}
}

// TestShardPutDeniedWithWrongIssuer proves a claimed Issuer that does not match
// the QUIC/TLS-authenticated remote peer for the stream is rejected — a node
// cannot present someone else's identity as the issuer of its own request.
func TestShardPutDeniedWithWrongIssuer(t *testing.T) {
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

	var putCalled, getCalled bool
	local := trackingShardServer{ShardServer: newMemShardServer(), putCalled: &putCalled, getCalled: &getCalled}
	worker.ServeShards(local, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	// A stranger third node mints its own valid cap; the requester claims that
	// stranger's PeerID as Issuer while dialing in under its OWN identity. The
	// wire's authenticated remote peer (from the worker's perspective) is
	// requester.PeerID(), which does not match the claimed Issuer.
	stranger, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("stranger: %v", err)
	}
	defer stranger.Close()

	env := mintShardCapAs(t, stranger, "test", contract.RightWrite, time.Hour)
	c := dummyCID(t, 0x44)
	if err := requester.RequestPutShard(ctx, worker.PeerID(), c.Bytes(), []byte("data"), env, stranger.PeerID()); err == nil {
		t.Fatal("put shard accepted a claimed issuer that does not match the authenticated stream peer")
	}
	if putCalled {
		t.Fatal("local.PutShard was invoked despite an issuer/stream mismatch")
	}
}

// TestShardPutDeniedWithExpiredCapability proves an otherwise well-formed,
// correctly self-signed capability is denied once expired.
func TestShardPutDeniedWithExpiredCapability(t *testing.T) {
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

	var putCalled, getCalled bool
	local := trackingShardServer{ShardServer: newMemShardServer(), putCalled: &putCalled, getCalled: &getCalled}
	worker.ServeShards(local, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	sc := shardSignerForFabric(t, requester)
	g, err := auth.NewGrant(MeshShardResource("test"), []contract.Right{contract.RightWrite}, nil, 0)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	g.NotBefore = time.Now().Add(-2 * time.Hour).Unix()
	g.Expiry = time.Now().Add(-time.Hour).Unix()
	env, err := sc.Issue(g)
	if err != nil {
		t.Fatalf("issue expired: %v", err)
	}

	c := dummyCID(t, 0x55)
	if err := requester.RequestPutShard(ctx, worker.PeerID(), c.Bytes(), []byte("data"), env, requester.PeerID()); err == nil {
		t.Fatal("put shard accepted an expired capability")
	}
	if putCalled {
		t.Fatal("local.PutShard was invoked despite an expired capability")
	}
}

// TestShardGetDeniedWithWriteOnlyCapability proves a capability minted with
// only RightWrite does not authorize ShardOpGet (rights are enforced per-op,
// not just "any valid cap").
func TestShardGetDeniedWithWriteOnlyCapability(t *testing.T) {
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

	local := newMemShardServer()
	c := dummyCID(t, 0x66)
	_ = local.PutShard(c.Bytes(), []byte("shard-bytes"))
	worker.ServeShards(local, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, nil)

	writeOnlyEnv := mintShardCapAs(t, requester, "test", contract.RightWrite, time.Hour)
	if _, err := requester.RequestGetShard(ctx, worker.PeerID(), c.Bytes(), writeOnlyEnv, requester.PeerID()); err == nil {
		t.Fatal("get shard accepted a write-only capability")
	}
}

// TestShardPutDeniedWithRevokedCapability proves a capability that verifies
// cryptographically but whose id the worker's revocation predicate flags is
// still denied before any local mutation.
func TestShardPutDeniedWithRevokedCapability(t *testing.T) {
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

	sc := shardSignerForFabric(t, requester)
	g, err := auth.NewGrant(MeshShardResource("test"), []contract.Right{contract.RightWrite}, nil, time.Hour)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	env, err := sc.Issue(g)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	pub, ok := SelfIssuerResolver(requester.PeerID())
	if !ok {
		t.Fatalf("SelfIssuerResolver failed to resolve requester's own PeerID")
	}
	verifiedOnce, err := auth.Verify(env, pub, time.Now().Unix(), nil)
	if err != nil {
		t.Fatalf("pre-check verify: %v", err)
	}
	isRevoked := func(id contract.CapID) bool { return id == verifiedOnce.ID }

	var putCalled, getCalled bool
	local := trackingShardServer{ShardServer: newMemShardServer(), putCalled: &putCalled, getCalled: &getCalled}
	worker.ServeShards(local, SelfIssuerResolver, func() int64 { return time.Now().Unix() }, isRevoked)

	c := dummyCID(t, 0x77)
	if err := requester.RequestPutShard(ctx, worker.PeerID(), c.Bytes(), []byte("data"), env, requester.PeerID()); err == nil {
		t.Fatal("put shard accepted a revoked capability")
	}
	if putCalled {
		t.Fatal("local.PutShard was invoked despite a revoked capability")
	}
}
