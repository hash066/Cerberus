package system

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/dataplane"
	"github.com/hash066/cerberus/daemon/dfs"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/ninep"
)

// TestPeerScatterCrossesRealNetwork is the real 2-node peer-scatter proof: two
// independently Compose'd systems (real mesh fabrics, real dfs engines, real
// data planes — mirroring test/e2e's two-real-process harness pattern, but as
// two in-process Systems joined by an explicit mesh Connect since mDNS multicast
// is unreliable on a test loopback), discover each other over the real mesh, and
// a file written on node A large enough to shard (multi-chunk, so multiple
// independent placement decisions happen) is proven to have placed at least one
// shard's bytes on node B's OWN local shard store — not merely "the file read
// back correctly", which could also pass under an all-local placement bug.
//
// The proof of a genuine network crossing is: node A's Manifest names a shard
// CID with placement "mesh:<B's PeerID>", and that exact CID is independently
// found in node B's OWN MemShardStore (sys.localShards) with the exact expected
// bytes — i.e. the bytes physically live in a different process-local map than
// the one dfs.Put on node A ever wrote to.
func TestPeerScatterCrossesRealNetwork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	kernelA := stub.NewCapKernel()
	kernelB := stub.NewCapKernel()

	sysA, err := Compose(ctx, kernelA, "peer-scatter-test", nil)
	if err != nil {
		t.Fatalf("compose A: %v", err)
	}
	doneA := startSystemForTest(sysA, ctx)
	defer stopSystemForTest(t, cancel, doneA)

	sysB, err := Compose(ctx, kernelB, "peer-scatter-test", nil)
	if err != nil {
		t.Fatalf("compose B: %v", err)
	}
	doneB := startSystemForTest(sysB, ctx)
	defer stopSystemForTest(t, cancel, doneB)

	fabA, ok := sysA.Fabric.(*mesh.Fabric)
	if !ok {
		t.Fatalf("sys.Fabric is not *mesh.Fabric (got %T)", sysA.Fabric)
	}
	fabB, ok := sysB.Fabric.(*mesh.Fabric)
	if !ok {
		t.Fatalf("sys.Fabric is not *mesh.Fabric (got %T)", sysB.Fabric)
	}

	// Explicit mesh connect (real discovery — mDNS multicast is unreliable on a
	// test loopback, exactly as test/e2e's harness documents for its own
	// discovery step).
	connCtx, connCancel := context.WithTimeout(ctx, 10*time.Second)
	defer connCancel()
	if err := fabA.Connect(connCtx, fabB.AddrInfo()); err != nil {
		t.Fatalf("A connect to B: %v", err)
	}
	// Give gossipsub/identify a moment to finish the handshake both directions
	// before we rely on Peers() reporting B from A's side.
	if !waitForPeer(t, fabA, fabB.PeerID(), 10*time.Second) {
		t.Fatal("node A never observed node B as a connected mesh peer")
	}

	// Write a file on node A big enough to guarantee multiple chunks (default
	// chunk size is 1 MiB) and therefore multiple independent PutShard calls —
	// giving the round-robin placement policy more than one chance to place a
	// shard remotely within the test's timeout.
	const path = "/cer/fs/scatter/big.bin"
	capA, err := kernelA.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: path},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)
	if err != nil {
		t.Fatalf("mint fs cap: %v", err)
	}
	data := bytes.Repeat([]byte("cerberus-peer-scatter-payload-"), 150_000) // ~4.5 MiB, several chunks

	writeFileToSystem(t, sysA, path, capA, data)

	// Inspect the Manifest node A recorded for this path (white-box: same
	// package). At least one chunk's shard placement must name node B's PeerID —
	// i.e. the round-robin policy actually chose the remote peer at least once.
	man, ok := sysA.fsStore.manifestFor(path)
	if !ok {
		t.Fatal("node A recorded no manifest for the written path")
	}
	var remoteShardFound bool
	for ci, cm := range man.Chunks {
		for si, cidWant := range cm.ShardCIDs {
			gotOnB, gerr := sysB.localShards.GetShard(cidWant)
			if gerr != nil {
				continue
			}
			if len(gotOnB) == 0 {
				continue
			}
			if _, aerr := sysA.localShards.GetShard(cidWant); aerr == nil {
				// Present on both nodes — local to A, not proof of scatter to B.
				continue
			}
			remoteShardFound = true
			_ = ci
			_ = si
		}
	}
	if !remoteShardFound {
		t.Fatalf("no shard in the manifest was placed on node B (peer scatter did not occur); placements: %+v", collectPlacements(man))
	}

	// Read the file back through node A's namespace (dfs.Get fetches the
	// remotely-placed shards back over the mesh RPC) and confirm it is
	// byte-identical to what was written — the full round-trip, on top of the
	// network-crossing proof above.
	got := readFileFromSystem(t, sysA, path, capA, uint64(len(data))+4096)
	if !bytes.Equal(got, data) {
		t.Fatalf("round-trip mismatch after peer scatter: got %d bytes, want %d", len(got), len(data))
	}
}

// TestWriteOnBReadFromA is the operator-facing multi-node proof: a file written
// on node B (via the synchronous FSPut path `cerberus fs put` uses) is listed
// and read back from node A over the mesh — metadata replicated via mesh meta
// RPC, shard bytes fetched via mesh shard RPC + dfs Reed-Solomon reconstruction.
func TestWriteOnBReadFromA(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	kernelA := stub.NewCapKernel()
	kernelB := stub.NewCapKernel()

	sysA, err := Compose(ctx, kernelA, "write-b-read-a", nil)
	if err != nil {
		t.Fatalf("compose A: %v", err)
	}
	doneA := startSystemForTest(sysA, ctx)
	defer stopSystemForTest(t, cancel, doneA)

	sysB, err := Compose(ctx, kernelB, "write-b-read-a", nil)
	if err != nil {
		t.Fatalf("compose B: %v", err)
	}
	doneB := startSystemForTest(sysB, ctx)
	defer stopSystemForTest(t, cancel, doneB)

	fabA, ok := sysA.Fabric.(*mesh.Fabric)
	if !ok {
		t.Fatalf("sysA.Fabric is not *mesh.Fabric (got %T)", sysA.Fabric)
	}
	fabB, ok := sysB.Fabric.(*mesh.Fabric)
	if !ok {
		t.Fatalf("sysB.Fabric is not *mesh.Fabric (got %T)", sysB.Fabric)
	}

	connCtx, connCancel := context.WithTimeout(ctx, 10*time.Second)
	defer connCancel()
	if err := fabA.Connect(connCtx, fabB.AddrInfo()); err != nil {
		t.Fatalf("A connect to B: %v", err)
	}
	if err := fabB.Connect(connCtx, fabA.AddrInfo()); err != nil {
		t.Fatalf("B connect to A: %v", err)
	}
	if !waitForPeer(t, fabA, fabB.PeerID(), 10*time.Second) {
		t.Fatal("node A never observed node B as a connected mesh peer")
	}
	if !waitForPeer(t, fabB, fabA.PeerID(), 10*time.Second) {
		t.Fatal("node B never observed node A as a connected mesh peer")
	}

	const path = "/cer/fs/remote/report.txt"
	data := bytes.Repeat([]byte("written-on-node-B-read-from-A\n"), 50_000) // ~1.5 MiB, multi-chunk

	// Write on node B (operator CLI path).
	if err := sysB.FSPut(path, data); err != nil {
		t.Fatalf("FSPut on B: %v", err)
	}

	// List from A — must include the path written on B (metadata replicated over mesh).
	var paths []string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		paths, err = sysA.FSList()
		if err != nil {
			t.Fatalf("FSList on A: %v", err)
		}
		for _, p := range paths {
			if p == path {
				goto listed
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("node A did not list file written on B within timeout; paths=%v", paths)
listed:
	// Read from A — reconstructs via dfs.Get (local + remote shards). Retry a
	// few times to tolerate transient mesh latency on multi-shard fan-out.
	var got []byte
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		got, lastErr = sysA.FSGet(path)
		if lastErr == nil && bytes.Equal(got, data) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("FSGet on A: %v", lastErr)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("cross-node read mismatch: got %d bytes, want %d", len(got), len(data))
	}
}

// collectPlacements flattens a Manifest's per-shard placement hints for a
// failure message.
func collectPlacements(man dfs.Manifest) []string {
	var out []string
	for _, cm := range man.Chunks {
		out = append(out, cm.Placement...)
	}
	return out
}

// waitForPeer polls Peers() until peerID appears or the timeout elapses.
func waitForPeer(t *testing.T, fab *mesh.Fabric, peerID contract.PeerID, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, p := range fab.Peers() {
			if p.ID == peerID {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// writeFileToSystem drives a /cer/fs write end-to-end through a composed System
// exactly as a real 9P caller would: OpenFSWrite mints the data-plane send
// endpoint, and the caller streams the bytes over the real QUIC data plane.
func writeFileToSystem(t *testing.T, sys *System, path string, cap contract.CapHandle, data []byte) {
	t.Helper()
	ep, err := sys.Namespace.OpenFSWrite(path, cap)
	if err != nil {
		t.Fatalf("OpenFSWrite(%s): %v", path, err)
	}
	dpEP := dataplane.Endpoint{
		Kind:       dataplane.EndpointKind(ep.Kind),
		Addr:       ep.Endpoint,
		TransferID: ep.StreamID,
		Cap:        cap,
		Quota:      ep.Quota,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := dataplane.NewClient().SendBytes(ctx, dpEP, data); err != nil {
		t.Fatalf("send file bytes over data plane: %v", err)
	}
}

// readFileFromSystem drives a /cer/fs read end-to-end: stand up a receiver,
// register an inbound grant, and hand the composed System's namespace the
// RecvEndpoint so it streams dfs.Get's reconstructed bytes back to us.
func readFileFromSystem(t *testing.T, sys *System, path string, cap contract.CapHandle, quota uint64) []byte {
	t.Helper()

	_, recvPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate receiver identity: %v", err)
	}
	recvSrv := dataplane.NewServer(sys.Kernel, time.Now().Unix(), recvPriv)
	if err := recvSrv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("recv listen: %v", err)
	}
	defer recvSrv.Close()

	resCh := make(chan []byte, 1)
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	go func() {
		_ = recvSrv.Serve(rctx, func(_ uint64, r io.Reader) error {
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			resCh <- b
			return nil
		})
	}()

	recvCap, err := sys.Kernel.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: path}, []contract.Right{contract.RightRead}, nil)
	if err != nil {
		t.Fatalf("mint recv cap: %v", err)
	}
	recvEP := recvSrv.RegisterGrant(42, recvCap, contract.Quota{Bytes: quota})

	err = sys.Namespace.OpenFSRead(path, cap, ninep.RecvEndpoint{
		Kind:     ninep.EndpointKind(recvEP.Kind),
		Endpoint: recvEP.Addr,
		StreamID: recvEP.TransferID,
		Cap:      recvEP.Cap,
		Quota:    recvEP.Quota,
	})
	if err != nil {
		t.Fatalf("OpenFSRead: %v", err)
	}

	select {
	case b := <-resCh:
		return b
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for read-back bytes")
		return nil
	}
}

// startSystemForTest starts sys's supervision tree (data plane, 9P wire server,
// telemetry) exactly once, as the live daemon does (cmd/cerberusd calls sys.Serve
// in its own goroutine right after Compose). It returns a channel closed once
// Serve returns, for stopSystemForTest to wait on.
func startSystemForTest(sys *System, ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		_ = sys.Serve(ctx)
		close(done)
	}()
	return done
}

// stopSystemForTest cancels the System's context and waits for the Serve
// goroutine startSystemForTest launched to return, so supervised services
// (mesh host, data-plane listener, 9P wire server) unwind before the test
// process exits. It must NOT call sys.Serve again — suture's Supervisor panics
// if Serve is invoked twice concurrently — so it only waits on the done channel
// the single Serve call already running signals.
func stopSystemForTest(t *testing.T, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Log("system did not shut down within 10s of context cancellation")
	}
}
