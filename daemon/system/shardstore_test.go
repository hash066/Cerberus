package system

// shardstore_test.go covers RemoteScatterShardStore in isolation against a fake
// meshFabric, proving:
//  1. PutShard/GetShard attach a real signed capability envelope to every
//     remote wire call (mintShardCap), rather than leaving it unauthenticated.
//  2. GetShard's local-miss fallback fans out CONCURRENTLY across known peers
//     and returns as soon as one succeeds, bounded by an OVERALL timeout — not
//     the old sequential per-peer timeout that made a genuine miss take up to
//     N*meshShardTimeout on an N-peer mesh. The fake fabric's RequestGetShard
//     sleeps a configurable duration per call so the test can assert the whole
//     GetShard call returns in much less than (numPeers * perPeerDelay).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/dfs"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

// fakeMeshFabric is an in-memory stand-in for *mesh.Fabric that records every
// RequestPutShard/RequestGetShard call (including the capability envelope and
// issuer it was presented with) so tests can assert on them directly, without
// standing up a real libp2p/QUIC mesh.
type fakeMeshFabric struct {
	self  contract.PeerID
	peers []contract.PeerInfo

	mu    sync.Mutex
	puts  []fakePutCall
	gets  []fakeGetCall
	delay time.Duration // artificial per-call latency, for fan-out timing tests

	// getResult, when non-nil, is returned by every RequestGetShard call that
	// doesn't hit getErrPeers; getErr is returned for peers listed in
	// getErrPeers (simulating "this peer doesn't have it").
	getResult  []byte
	getErrPeer map[contract.PeerID]bool
}

type fakePutCall struct {
	peer   contract.PeerID
	cap    []byte
	issuer contract.PeerID
}

type fakeGetCall struct {
	peer   contract.PeerID
	cap    []byte
	issuer contract.PeerID
}

func (f *fakeMeshFabric) Peers() []contract.PeerInfo { return f.peers }
func (f *fakeMeshFabric) PeerID() contract.PeerID    { return f.self }

func (f *fakeMeshFabric) RequestPutShard(ctx context.Context, peer contract.PeerID, cidBytes []byte, shard []byte, capEnvelope []byte, issuer contract.PeerID) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	f.puts = append(f.puts, fakePutCall{peer: peer, cap: capEnvelope, issuer: issuer})
	f.mu.Unlock()
	if len(capEnvelope) == 0 {
		return errors.New("fake: no capability presented")
	}
	return nil
}

func (f *fakeMeshFabric) RequestGetShard(ctx context.Context, peer contract.PeerID, cidBytes []byte, capEnvelope []byte, issuer contract.PeerID) ([]byte, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	f.gets = append(f.gets, fakeGetCall{peer: peer, cap: capEnvelope, issuer: issuer})
	f.mu.Unlock()
	if len(capEnvelope) == 0 {
		return nil, errors.New("fake: no capability presented")
	}
	if f.getErrPeer != nil && f.getErrPeer[peer] {
		return nil, fmt.Errorf("fake: peer %x does not have the shard", peer[:4])
	}
	if f.getResult != nil {
		return f.getResult, nil
	}
	return nil, fmt.Errorf("fake: peer %x does not have the shard", peer[:4])
}

var _ meshFabric = (*fakeMeshFabric)(nil)

func newTestSigner(t *testing.T) (*auth.SignedCap, contract.PeerID) {
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
	id, err := sc.IssuerPeerID()
	if err != nil {
		t.Fatalf("issuer id: %v", err)
	}
	return sc, id
}

func testShardCID(t *testing.T, seed byte) cid.Cid {
	t.Helper()
	mh, err := multihash.Sum([]byte{seed, seed, seed, seed}, multihash.SHA2_256, -1)
	if err != nil {
		t.Fatalf("build cid: %v", err)
	}
	return cid.NewCidV1(cid.Raw, mh)
}

func fakePeerID(n byte) contract.PeerID {
	var id contract.PeerID
	id[0] = n
	return id
}

// TestPutShardAttachesRealCapability proves PutShard's remote wire call
// carries a non-empty, correctly-scoped (RightWrite) signed capability
// envelope naming this store's own issuer — i.e. the client actually makes the
// server-side capability gate meaningful, rather than leaving Cap empty.
func TestPutShardAttachesRealCapability(t *testing.T) {
	signer, issuerID := newTestSigner(t)
	fake := &fakeMeshFabric{self: fakePeerID(1), peers: []contract.PeerInfo{{ID: fakePeerID(2)}}}
	store := NewRemoteScatterShardStore(dfs.NewMemShardStore(), fake, signer, "test-site")

	// Force a remote placement on the very first call: placementEvery=3, so call
	// PutShard 3 times; the 3rd is the one that attempts remote placement.
	c1, c2, c3 := testShardCID(t, 1), testShardCID(t, 2), testShardCID(t, 3)
	if _, err := store.PutShard(c1, []byte("a")); err != nil {
		t.Fatalf("put 1: %v", err)
	}
	if _, err := store.PutShard(c2, []byte("b")); err != nil {
		t.Fatalf("put 2: %v", err)
	}
	placement, err := store.PutShard(c3, []byte("c"))
	if err != nil {
		t.Fatalf("put 3: %v", err)
	}
	if placement == "" || placement[:len(remotePlacementPrefix)] != remotePlacementPrefix {
		t.Fatalf("3rd put did not scatter remotely as expected, placement=%q", placement)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.puts) != 1 {
		t.Fatalf("expected exactly 1 remote put call, got %d", len(fake.puts))
	}
	call := fake.puts[0]
	if len(call.cap) == 0 {
		t.Fatal("remote PutShard call carried an EMPTY capability envelope — server-side gate would deny it")
	}
	if call.issuer != issuerID {
		t.Fatalf("remote PutShard call named issuer %x, want the store's own signer issuer %x", call.issuer[:4], issuerID[:4])
	}
	// The envelope must actually verify under the issuer's own key and convey
	// RightWrite on MeshShardResource("test-site") — not just be non-empty bytes.
	pub, ok := SelfIssuerResolverPublicKey(t, signer)
	if !ok {
		t.Fatal("could not recover signer public key")
	}
	grant, err := auth.Verify(call.cap, pub, time.Now().Unix(), nil)
	if err != nil {
		t.Fatalf("attached capability does not verify: %v", err)
	}
	if !hasRight(grant.Rights, contract.RightWrite) {
		t.Fatalf("attached capability rights = %v, want RightWrite", grant.Rights)
	}
}

// TestGetShardAttachesRealCapability is PutShard's sibling for the read path:
// a local miss must mint and attach a RightRead capability before asking any
// peer.
func TestGetShardAttachesRealCapability(t *testing.T) {
	signer, issuerID := newTestSigner(t)
	fake := &fakeMeshFabric{
		self:      fakePeerID(1),
		peers:     []contract.PeerInfo{{ID: fakePeerID(2)}},
		getResult: []byte("remote-shard-bytes"),
	}
	store := NewRemoteScatterShardStore(dfs.NewMemShardStore(), fake, signer, "test-site")

	c := testShardCID(t, 9)
	got, err := store.GetShard(c)
	if err != nil {
		t.Fatalf("GetShard: %v", err)
	}
	if string(got) != "remote-shard-bytes" {
		t.Fatalf("got %q", got)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.gets) == 0 {
		t.Fatal("no remote get call recorded")
	}
	call := fake.gets[0]
	if len(call.cap) == 0 {
		t.Fatal("remote GetShard call carried an EMPTY capability envelope")
	}
	if call.issuer != issuerID {
		t.Fatalf("remote GetShard call named issuer %x, want %x", call.issuer[:4], issuerID[:4])
	}
}

// TestPutShardNilSignerNeverSendsUnauthenticatedRequest proves that with no
// signer configured, PutShard NEVER makes a remote wire call at all — it falls
// back to purely local placement rather than ever presenting an
// unauthenticated request (fail closed on missing configuration).
func TestPutShardNilSignerNeverSendsUnauthenticatedRequest(t *testing.T) {
	fake := &fakeMeshFabric{self: fakePeerID(1), peers: []contract.PeerInfo{{ID: fakePeerID(2)}}}
	store := NewRemoteScatterShardStore(dfs.NewMemShardStore(), fake, nil, "test-site")

	for i, seed := range []byte{1, 2, 3, 4, 5, 6} {
		if _, err := store.PutShard(testShardCID(t, seed), []byte{seed}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.puts) != 0 {
		t.Fatalf("expected 0 remote put calls with no signer configured, got %d", len(fake.puts))
	}
}

// TestGetShardFanoutIsConcurrentNotSequential is the concrete timing proof for
// the fan-out fix: with many known peers, each artificially slow, a genuine
// miss (none of them has the shard) must return in roughly ONE peer's delay,
// not (numPeers * delay) — proving peers are queried concurrently, not in a
// sequential loop with a per-peer timeout.
func TestGetShardFanoutIsConcurrentNotSequential(t *testing.T) {
	const numPeers = 6
	const perPeerDelay = 300 * time.Millisecond

	signer, _ := newTestSigner(t)
	peers := make([]contract.PeerInfo, 0, numPeers)
	errPeers := map[contract.PeerID]bool{}
	for i := 0; i < numPeers; i++ {
		id := fakePeerID(byte(10 + i))
		peers = append(peers, contract.PeerInfo{ID: id})
		errPeers[id] = true // none of them has the shard
	}
	fake := &fakeMeshFabric{self: fakePeerID(1), peers: peers, delay: perPeerDelay, getErrPeer: errPeers}
	store := NewRemoteScatterShardStore(dfs.NewMemShardStore(), fake, signer, "test-site")

	c := testShardCID(t, 42)
	start := time.Now()
	_, err := store.GetShard(c)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("GetShard succeeded despite no peer (and no local copy) having the shard")
	}
	// Sequential-per-peer behaviour would take numPeers*perPeerDelay (1.8s here).
	// Concurrent fan-out should take roughly one perPeerDelay (plus scheduling
	// slack); generously allow up to 3x a single delay but well under the
	// sequential total, to keep this robust on a loaded CI box.
	sequentialWorstCase := time.Duration(numPeers) * perPeerDelay
	if elapsed >= sequentialWorstCase {
		t.Fatalf("GetShard took %s, expected well under the sequential worst case %s — fan-out is not concurrent", elapsed, sequentialWorstCase)
	}
	if elapsed > 3*perPeerDelay {
		t.Fatalf("GetShard took %s, expected close to a single peer delay (%s) under concurrent fan-out", elapsed, perPeerDelay)
	}

	fake.mu.Lock()
	gotCalls := len(fake.gets)
	fake.mu.Unlock()
	if gotCalls != numPeers {
		t.Fatalf("expected all %d known peers to be queried, got %d calls", numPeers, gotCalls)
	}
}

// TestGetShardFanoutCancelsOnFirstSuccess proves that once one peer succeeds,
// GetShard returns immediately rather than waiting for the others — a slow
// "loser" peer must not hold up a successful fan-out.
func TestGetShardFanoutCancelsOnFirstSuccess(t *testing.T) {
	signer, _ := newTestSigner(t)
	fastPeer := fakePeerID(20)
	slowPeer := fakePeerID(21)
	peers := []contract.PeerInfo{{ID: slowPeer}, {ID: fastPeer}}

	// A custom fake that answers fastPeer immediately and slowPeer after a long
	// delay, so we can prove GetShard doesn't wait for the slow one.
	fake := &raceFakeFabric{self: fakePeerID(1), peers: peers, fast: fastPeer, slow: slowPeer, slowDelay: 5 * time.Second, result: []byte("fast-bytes")}
	store := NewRemoteScatterShardStore(dfs.NewMemShardStore(), fake, signer, "test-site")

	c := testShardCID(t, 7)
	start := time.Now()
	got, err := store.GetShard(c)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GetShard: %v", err)
	}
	if string(got) != "fast-bytes" {
		t.Fatalf("got %q, want fast-bytes", got)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("GetShard took %s — did not return promptly on the fast peer's success", elapsed)
	}
}

// raceFakeFabric answers one peer immediately and another only after a long
// delay, for TestGetShardFanoutCancelsOnFirstSuccess.
type raceFakeFabric struct {
	self      contract.PeerID
	peers     []contract.PeerInfo
	fast      contract.PeerID
	slow      contract.PeerID
	slowDelay time.Duration
	result    []byte

	calls int32
}

func (r *raceFakeFabric) Peers() []contract.PeerInfo { return r.peers }
func (r *raceFakeFabric) PeerID() contract.PeerID    { return r.self }

func (r *raceFakeFabric) RequestPutShard(ctx context.Context, peer contract.PeerID, cidBytes []byte, shard []byte, capEnvelope []byte, issuer contract.PeerID) error {
	return errors.New("not used in this test")
}

func (r *raceFakeFabric) RequestGetShard(ctx context.Context, peer contract.PeerID, cidBytes []byte, capEnvelope []byte, issuer contract.PeerID) ([]byte, error) {
	atomic.AddInt32(&r.calls, 1)
	if len(capEnvelope) == 0 {
		return nil, errors.New("fake: no capability presented")
	}
	if peer == r.fast {
		return r.result, nil
	}
	select {
	case <-time.After(r.slowDelay):
		return r.result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var _ meshFabric = (*raceFakeFabric)(nil)

func hasRight(rights []contract.Right, want contract.Right) bool {
	for _, r := range rights {
		if r == want {
			return true
		}
	}
	return false
}

// SelfIssuerResolverPublicKey recovers the Ed25519 public key from a SignedCap
// issuer for test-side verification, since *auth.SignedCap does not expose the
// underlying key directly — only IssuerPeerID (which IS the public key bytes).
func SelfIssuerResolverPublicKey(t *testing.T, sc *auth.SignedCap) (ed25519.PublicKey, bool) {
	t.Helper()
	id, err := sc.IssuerPeerID()
	if err != nil {
		return nil, false
	}
	return ed25519.PublicKey(append([]byte(nil), id[:]...)), true
}
