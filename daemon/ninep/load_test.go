package ninep_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/dataplane"
	"github.com/hash066/cerberus/daemon/ninep"
)

// TestConcurrentCtlGrantsAndTransfers is a load test for the 9P↔data-plane
// bridge: many holders concurrently open the same device's ctl (each minting its
// own capability and a distinct transfer) and push a blob over the data plane at
// the same time. It asserts every transfer is authorized and delivered, and that
// the exact total byte count arrives — exercising RegisterGrant under contention
// and the server's concurrent per-stream handling.
func TestConcurrentCtlGrantsAndTransfers(t *testing.T) {
	const (
		dev      = "/cer/dev/vram/AA/0"
		workers  = 24
		blobSize = 4096
	)
	q := contract.Quota{Bytes: 1 << 20}
	ref := contract.ResourceRef{Kind: contract.KindVRAM, Path: dev, Quota: &q}

	kernel := stub.NewCapKernel()
	dp := dataplane.NewServer(kernel, time.Now().Unix(), newTestIdentity(t))
	if err := dp.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("dataplane listen: %v", err)
	}
	defer dp.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var received int64
	go func() {
		_ = dp.Serve(ctx, func(_ uint64, r io.Reader) error {
			n, err := io.Copy(io.Discard, r)
			if err != nil {
				return err
			}
			atomic.AddInt64(&received, n)
			return nil
		})
	}()

	ns := ninep.New(kernel)
	ns.Register(dev, ref)
	ns.SetGranter(func(cap contract.CapHandle, _ contract.ResourceRef, transferID uint64, quota contract.Quota) (ninep.DataEndpoint, error) {
		ep := dp.RegisterGrant(transferID, cap, quota)
		return ninep.DataEndpoint{Kind: ninep.EndpointKind(ep.Kind), Endpoint: ep.Addr, StreamID: ep.TransferID, Quota: ep.Quota, ServerPeerID: ep.ServerPeerID}, nil
	})

	client := dataplane.NewClient()
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cap, err := kernel.Mint(ref, []contract.Right{contract.RightRead, contract.RightAlloc}, nil)
			if err != nil {
				errs <- fmt.Errorf("worker %d mint: %w", i, err)
				return
			}
			ep, err := ns.Open(dev+"/ctl", cap)
			if err != nil {
				errs <- fmt.Errorf("worker %d open: %w", i, err)
				return
			}
			dpEP := dataplane.Endpoint{Kind: dataplane.EndpointQUIC, Addr: ep.Endpoint, TransferID: ep.StreamID, Cap: cap, Quota: ep.Quota, ServerPeerID: ep.ServerPeerID}
			sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
			defer scancel()
			if err := client.SendBytes(sctx, dpEP, bytes.Repeat([]byte{byte(i)}, blobSize)); err != nil {
				errs <- fmt.Errorf("worker %d send: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// SendBytes returns only after the server's sink consumed and acked the
	// transfer, so every worker's bytes are accounted for by the time wg unblocks.
	if got := atomic.LoadInt64(&received); got != int64(workers*blobSize) {
		t.Fatalf("received %d bytes, want %d", got, workers*blobSize)
	}
}
