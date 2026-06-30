package node

import (
	"context"
	"io"
	"log"
	"strconv"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/wasm"
)

func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(io.Discard, "", 0)
}

// newTestNode builds a minimal server backed by a real fabric + content store,
// without the HTTP layer, so the mesh compute path can be unit-tested directly.
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
	s := &server{
		self:   Peer{ID: id, MeshPeerID: encodePeerID(fab.PeerID())},
		kernel: k,
		fabric: fab,
		store:  cstore,
		log:    testLogger(t),
		peers:  map[string]Peer{},
	}
	fab.ServeCompute(s.handleCompute)
	return s
}

// TestMeshComputeRoundTrip runs the real dispatch path: the requester
// content-addresses the hello-shard, sends the CID over the mesh, and the worker
// resolves the CID from its store (integrity-checked), runs the wasm, and returns
// 1337. The worker authorizes the requester-presented cap against its own kernel.
func TestMeshComputeRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester := newTestNode(t, ctx, "requester")
	worker := newTestNode(t, ctx, "worker")

	if err := requester.fabric.Connect(ctx, worker.fabric.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Worker grants the requester an exec cap on its own kernel.
	grant, err := worker.grantExecCap()
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	// Requester content-addresses the component and dispatches the CID.
	c, err := requester.store.Put(HelloShardWASM())
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	task := contract.ComputeTask{TaskID: []byte("task-1"), Component: c.Bytes()}

	res, err := requester.fabric.RequestCompute(ctx, worker.fabric.PeerID(), task, grant)
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
// is not in its content store (no unverifiable payload is executed).
func TestMeshComputeUnknownCID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	worker := newTestNode(t, ctx, "worker")
	grant, err := worker.grantExecCap()
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	// A CID for bytes the worker's store does not hold.
	unknown, err := wasm.ComponentCID([]byte("not-the-hello-shard"))
	if err != nil {
		t.Fatal(err)
	}
	res, herr := worker.handleCompute(ctx, contract.ComputeTask{TaskID: []byte("t"), Component: unknown.Bytes()}, grant)
	if herr != nil {
		t.Fatalf("handler error: %v", herr)
	}
	if res.OK {
		t.Fatal("worker executed an unresolvable CID")
	}
}

// TestHandleComputeDeniesBadCap proves the capability gate: a zero/unknown cap is
// denied before any execution.
func TestHandleComputeDeniesBadCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	worker := newTestNode(t, ctx, "worker")
	c, _ := worker.store.Put(HelloShardWASM())
	res, _ := worker.handleCompute(ctx, contract.ComputeTask{TaskID: []byte("t"), Component: c.Bytes()}, contract.CapHandle(0))
	if res.OK {
		t.Fatal("worker ran a task with no capability")
	}
}
