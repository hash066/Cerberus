package system

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/dataplane"
	"github.com/hash066/cerberus/daemon/ninep"
)

// fsFixture stands up the /cer/fs bridge exactly as Compose does — a real dfs
// engine + a real QUIC data plane behind the ninep namespace's FSStore seam — but
// without the libp2p mesh, so the round-trip is fast and deterministic. It returns
// the namespace, the kernel, and a stop func.
type fsFixture struct {
	ns     *ninep.Server
	kernel *stub.CapKernel
	dp     *dataplane.Server
	stop   func()
}

func newFSFixture(t *testing.T) *fsFixture {
	t.Helper()
	kernel := stub.NewCapKernel()

	// Daemon-side data-plane receiver + routing sink (the write path streams into
	// dfs.Put through the router).
	dp := dataplane.NewServer(kernel, time.Now().Unix())
	if err := dp.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("dataplane listen: %v", err)
	}
	router := newSinkRouter()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = dp.Serve(ctx, router.route); close(done) }()

	fsStore, err := newDFSFSStore(dp, router, nil, nil)
	if err != nil {
		t.Fatalf("dfs fs store: %v", err)
	}
	ns := ninep.New(kernel)
	ns.SetFSStore(fsStore)

	return &fsFixture{
		ns:     ns,
		kernel: kernel,
		dp:     dp,
		stop: func() {
			cancel()
			_ = dp.Close()
			<-done
		},
	}
}

// writeFile drives a /cer/fs write end-to-end: OpenFSWrite mints the send
// endpoint on the daemon's data plane, and the caller streams the file bytes over
// the data plane into dfs.Put. Bytes never touch 9P.
func writeFile(t *testing.T, f *fsFixture, path string, cap contract.CapHandle, data []byte) {
	t.Helper()
	ep, err := f.ns.OpenFSWrite(path, cap)
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := dataplane.NewClient().SendBytes(ctx, dpEP, data); err != nil {
		t.Fatalf("send file bytes over data plane: %v", err)
	}
}

// readFile drives a /cer/fs read end-to-end: the caller stands up its OWN
// data-plane receiver (a read makes the daemon the sender), registers an inbound
// grant, and hands the daemon that RecvEndpoint. The daemon runs dfs.Get and
// streams the reconstructed bytes to the caller's receiver. Bytes never touch 9P.
func readFile(t *testing.T, f *fsFixture, path string, cap contract.CapHandle, transferID uint64, quota uint64) ([]byte, error) {
	t.Helper()

	// Caller's own receiver.
	recvSrv := dataplane.NewServer(f.kernel, time.Now().Unix())
	if err := recvSrv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("recv listen: %v", err)
	}
	defer recvSrv.Close()

	var mu sync.Mutex
	var got []byte
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	served := make(chan struct{})
	go func() {
		_ = recvSrv.Serve(rctx, func(_ uint64, r io.Reader) error {
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			mu.Lock()
			got = b
			mu.Unlock()
			return nil
		})
		close(served)
	}()

	// Caller mints its own inbound cap and registers the grant on its receiver.
	recvCap, err := f.kernel.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: path}, []contract.Right{contract.RightRead}, nil)
	if err != nil {
		t.Fatalf("mint recv cap: %v", err)
	}
	recvEP := recvSrv.RegisterGrant(transferID, recvCap, contract.Quota{Bytes: quota})

	// The daemon streams dfs.Get's output to the caller's receiver.
	err = f.ns.OpenFSRead(path, cap, ninep.RecvEndpoint{
		Kind:     ninep.EndpointKind(recvEP.Kind),
		Endpoint: recvEP.Addr,
		StreamID: recvEP.TransferID,
		Cap:      recvEP.Cap,
		Quota:    recvEP.Quota,
	})
	if err != nil {
		return nil, err
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]byte(nil), got...), nil
}

// fsCap mints a read+write capability for a /cer/fs path.
func fsCap(t *testing.T, k *stub.CapKernel, path string) contract.CapHandle {
	t.Helper()
	cap, err := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: path},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)
	if err != nil {
		t.Fatalf("mint fs cap: %v", err)
	}
	return cap
}

// TestFSRoundTripThroughNamespace is the headline deliverable: write a file to
// /cer/fs and read it back, with the bytes flowing through the real dfs engine
// (chunk → Reed-Solomon → content-addressed shards) over the real QUIC data plane,
// driven through the 9P namespace API. The reconstructed bytes must equal exactly
// what was written. This proves /cer/fs is real end-to-end.
func TestFSRoundTripThroughNamespace(t *testing.T) {
	f := newFSFixture(t)
	defer f.stop()

	const path = "/cer/fs/docs/hello.txt"
	cap := fsCap(t, f.kernel, path)

	// A payload that spans multiple dfs chunks (default chunk is 1 MiB) to exercise
	// the multi-chunk erasure path, plus a partial last chunk.
	data := bytes.Repeat([]byte("cerberus-distributed-fs-"), 100_000) // ~2.4 MiB
	writeFile(t, f, path, cap, data)

	got, err := readFile(t, f, path, cap, 1, uint64(len(data))+1024)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("round-trip mismatch: read %d bytes, wrote %d (equal=%v)", len(got), len(data), bytes.Equal(got, data))
	}
}

// TestFSRoundTripSmall covers a sub-chunk file to make sure short files round-trip
// too (the partial-last-chunk padding path in dfs).
func TestFSRoundTripSmall(t *testing.T) {
	f := newFSFixture(t)
	defer f.stop()

	const path = "/cer/fs/a.txt"
	cap := fsCap(t, f.kernel, path)
	data := []byte("a small file that fits well within one dfs chunk")
	writeFile(t, f, path, cap, data)

	got, err := readFile(t, f, path, cap, 1, uint64(len(data))+1024)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("round-trip mismatch: got %q, want %q", got, data)
	}
}

// TestFSReadUnknownPathFails proves a read of a path that was never written is
// denied — the namespace does not fabricate bytes for a missing file.
func TestFSReadUnknownPathFails(t *testing.T) {
	f := newFSFixture(t)
	defer f.stop()

	const path = "/cer/fs/never/written.bin"
	cap := fsCap(t, f.kernel, path)

	_, err := readFile(t, f, path, cap, 1, 4096)
	if err == nil {
		t.Fatal("read of an unknown /cer/fs path must fail")
	}
	var ce *contract.CapError
	if !errors.As(err, &ce) || ce.Code != contract.ErrDenied {
		t.Fatalf("expected DENIED for unknown path, got %v", err)
	}
}

// TestFSCaplessAccessDenied proves no ambient authority: a write or read without a
// valid capability is denied before any grant is minted or any byte flows.
func TestFSCaplessAccessDenied(t *testing.T) {
	f := newFSFixture(t)
	defer f.stop()

	const path = "/cer/fs/secret.bin"

	// Handle 0 was never minted — the kernel rejects it.
	if _, err := f.ns.OpenFSWrite(path, contract.CapHandle(0)); err == nil {
		t.Fatal("write without a capability must be denied")
	}
	if err := f.ns.WalkFS(path, contract.CapHandle(0)); err == nil {
		t.Fatal("walk to a file without a capability must be denied")
	}

	// A revoked capability is likewise denied.
	cap := fsCap(t, f.kernel, path)
	if err := f.kernel.Revoke(cap); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := f.ns.OpenFSWrite(path, cap); err == nil {
		t.Fatal("write with a revoked capability must be denied")
	}
	if err := f.ns.OpenFSRead(path, cap, ninep.RecvEndpoint{}); err == nil {
		t.Fatal("read with a revoked capability must be denied")
	}
}
