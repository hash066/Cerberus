package node

import (
	"context"
	"strconv"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/wasm"
)

// newTestNodeNoSeed is newTestNode but WITHOUT pre-populating the content
// store with hello-shard — used to prove the peer-fetch path is what actually
// supplies the bytes, not a coincidence of both nodes embedding them.
func newTestNodeNoSeed(t *testing.T, ctx context.Context, id string) *server {
	t.Helper()
	s := newTestNode(t, ctx, id)
	// newTestNode already seeded the store; rebuild a fresh, empty one in its
	// place so this node genuinely starts without the component.
	s.store = wasm.NewContentStore()
	s.fabric.ServeComponentFetch(
		s.store,
		s.resolveIssuerKey,
		func() int64 { return time.Now().Unix() },
		nil,
	)
	return s
}

// TestWorkerFetchesMissingComponentFromPeer is the concrete proof the p2p
// component-fetch path works: node A has hello-shard registered locally; node
// B does NOT. We dispatch a signed-compute task on B naming hello-shard's CID.
// B's local cidstore lookup misses, so it must fall back to
// fetchComponentFromPeers and pull the real bytes from A over the mesh,
// verify their hash, execute them, and return the correct value — not merely
// happen to already have the bytes (B's store starts empty; this is the
// opposite of today's demo setup, where both nodes embed identical bytes).
func TestWorkerFetchesMissingComponentFromPeer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nodeA := newTestNode(t, ctx, "node-a") // has hello-shard
	nodeB := newTestNodeNoSeed(t, ctx, "node-b") // does NOT have hello-shard

	// Symmetric trust + mesh connectivity, exactly as discovery would establish.
	nodeA.trustPeer(t, nodeB)
	nodeB.trustPeer(t, nodeA)
	if err := nodeA.fabric.Connect(ctx, nodeB.fabric.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Register each as a "known peer" of the other (what discovery/addPeer does
	// in production), so fetchComponentFromPeers has a candidate to try.
	nodeA.mu.Lock()
	nodeA.peers[nodeB.self.ID] = nodeB.self
	nodeA.mu.Unlock()
	nodeB.mu.Lock()
	nodeB.peers[nodeA.self.ID] = nodeA.self
	nodeB.mu.Unlock()

	// Precondition: B's store genuinely does not have hello-shard's CID yet.
	helloCID, err := wasm.ComponentCID(HelloShardWASM())
	if err != nil {
		t.Fatalf("hash hello-shard: %v", err)
	}
	if nodeB.store.Has(helloCID) {
		t.Fatal("test setup bug: node B already has the component locally")
	}

	// Requester (node A, which dispatches the task) grants node B the exec cap,
	// since B is the worker in this round-trip.
	_, env, err := nodeB.grantExecCap()
	if err != nil {
		t.Fatalf("worker grant: %v", err)
	}

	// A dispatches hello-shard's CID (not the bytes) to B over the signed
	// compute path. B must resolve the CID itself — locally miss, then fetch
	// from A.
	task := contract.ComputeTask{
		TaskID:    []byte("fetch-proof-task"),
		Component: helloCID.Bytes(),
		Caps:      [][]byte{env},
	}
	res, err := nodeA.fabric.RequestComputeSigned(ctx, nodeB.fabric.PeerID(), task, nodeB.issuerID, 0)
	if err != nil {
		t.Fatalf("request compute signed: %v", err)
	}
	if !res.OK {
		t.Fatalf("node B failed to resolve+run the component it did not have locally: %s", res.Error)
	}
	v, err := strconv.Atoi(string(res.Output))
	if err != nil {
		t.Fatalf("non-integer output %q: %v", res.Output, err)
	}
	if v != HelloShardValue {
		t.Fatalf("value = %d, want %d", v, HelloShardValue)
	}

	// Concrete evidence: B's local store now holds the CID because the fetch
	// path populated it — a SUBSEQUENT lookup is now a genuine local hit.
	if !nodeB.store.Has(helloCID) {
		t.Fatal("node B's content store was not populated by the peer fetch")
	}
	cached, err := nodeB.store.Get(helloCID)
	if err != nil {
		t.Fatalf("node B cannot resolve the CID locally after the fetch: %v", err)
	}
	if string(cached) != string(HelloShardWASM()) {
		t.Fatal("node B's cached bytes do not match hello-shard")
	}
}

// TestWorkerFetchFailsCleanlyWhenNoPeerHasComponent proves the failure path:
// if the CID is not in the worker's local store AND no known peer has it
// either, the task fails with a clear error — it must not hang or panic.
func TestWorkerFetchFailsCleanlyWhenNoPeerHasComponent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nodeA := newTestNodeNoSeed(t, ctx, "node-a")
	nodeB := newTestNodeNoSeed(t, ctx, "node-b")

	nodeA.trustPeer(t, nodeB)
	nodeB.trustPeer(t, nodeA)
	if err := nodeA.fabric.Connect(ctx, nodeB.fabric.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	nodeA.mu.Lock()
	nodeA.peers[nodeB.self.ID] = nodeB.self
	nodeA.mu.Unlock()
	nodeB.mu.Lock()
	nodeB.peers[nodeA.self.ID] = nodeA.self
	nodeB.mu.Unlock()

	_, env, err := nodeB.grantExecCap()
	if err != nil {
		t.Fatalf("worker grant: %v", err)
	}

	// A CID that genuinely nobody has.
	unknownCID, err := wasm.ComponentCID([]byte("nobody-in-the-mesh-has-this"))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	task := contract.ComputeTask{TaskID: []byte("t"), Component: unknownCID.Bytes(), Caps: [][]byte{env}}

	done := make(chan struct{})
	var res contract.ComputeResult
	var rerr error
	go func() {
		defer close(done)
		res, rerr = nodeA.fabric.RequestComputeSigned(ctx, nodeB.fabric.PeerID(), task, nodeB.issuerID, 0)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("dispatch of an unresolvable CID hung instead of failing cleanly")
	}
	if rerr != nil {
		t.Fatalf("transport error: %v", rerr)
	}
	if res.OK {
		t.Fatal("worker reported success for a CID no peer actually has")
	}
	if res.Error == "" {
		t.Fatal("worker failure carries no error message")
	}
}
