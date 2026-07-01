package ninep_test

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
	"github.com/hash066/cerberus/daemon/ninep"
)

// newTestIdentity generates a fresh Ed25519 keypair standing in for a node's
// real mesh identity, matching daemon/system.Compose's wiring (dp :=
// dataplane.NewServer(kernel, now, fab.Identity())) without requiring a live
// mesh fabric in these fast, deterministic bridge tests. Shared by every
// ninep_test file that stands up a dataplane.Server.
func newTestIdentity(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen identity: %v", err)
	}
	return priv
}

// TestOpenCtlGrantsRealDataPlaneTransfer proves the cross-cut wiring (HANDOFF
// Phase F "next"): opening a device `.../ctl` on the 9P namespace allocates a
// real, capability-bound transfer on the QUIC data plane and returns the endpoint
// a holder dials. The holder then moves bytes over the data plane within the
// grant's quota — bytes never traversing 9P — and is rejected past the quota.
//
// This is the same bridge daemon/system.Compose installs in the live daemon,
// exercised here without the libp2p mesh so it is fast and deterministic.
func TestOpenCtlGrantsRealDataPlaneTransfer(t *testing.T) {
	const dev = "/cer/dev/vram/AA/0"
	q := contract.Quota{Bytes: 1024}
	ref := contract.ResourceRef{Kind: contract.KindVRAM, Path: dev, Quota: &q}

	kernel := stub.NewCapKernel()
	now := time.Now().Unix()

	// Data plane: real QUIC receiver.
	dp := dataplane.NewServer(kernel, now, newTestIdentity(t))
	if err := dp.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("dataplane listen: %v", err)
	}
	defer dp.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received := make(chan []byte, 1)
	go func() {
		_ = dp.Serve(ctx, func(_ uint64, r io.Reader) error {
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			received <- b
			return nil
		})
	}()

	// Control plane: the namespace, bridged to the data plane exactly as Compose does.
	ns := ninep.New(kernel)
	ns.Register(dev, ref)
	ns.SetGranter(func(cap contract.CapHandle, _ contract.ResourceRef, transferID uint64, quota contract.Quota) (ninep.DataEndpoint, error) {
		ep := dp.RegisterGrant(transferID, cap, quota)
		return ninep.DataEndpoint{
			Kind:         ninep.EndpointKind(ep.Kind),
			Endpoint:     ep.Addr,
			StreamID:     ep.TransferID,
			Quota:        ep.Quota,
			ServerPeerID: ep.ServerPeerID,
		}, nil
	})

	cap, err := kernel.Mint(ref, []contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	if err != nil {
		t.Fatalf("mint cap: %v", err)
	}

	// Open ctl -> a real, dialable data-plane endpoint (not the placeholder).
	ep, err := ns.Open(dev+"/ctl", cap)
	if err != nil {
		t.Fatalf("open ctl: %v", err)
	}
	if ep.Endpoint != dp.Addr() {
		t.Fatalf("ctl must return the live data-plane addr %q, got placeholder/other %q", dp.Addr(), ep.Endpoint)
	}
	if ep.Quota.Bytes != q.Bytes {
		t.Fatalf("endpoint must carry the grant quota %d, got %d", q.Bytes, ep.Quota.Bytes)
	}

	client := dataplane.NewClient()

	// Within quota: the transfer succeeds and the bytes arrive over the data plane.
	// ServerPeerID pins this dial to the daemon's real identity (the same
	// pattern daemon/system.Compose wires end-to-end).
	blob := bytes.Repeat([]byte("x"), 512)
	dpEP := dataplane.Endpoint{Kind: dataplane.EndpointQUIC, Addr: ep.Endpoint, TransferID: ep.StreamID, Cap: cap, Quota: ep.Quota, ServerPeerID: ep.ServerPeerID}
	sendCtx, sendCancel := context.WithTimeout(ctx, 5*time.Second)
	defer sendCancel()
	if err := client.SendBytes(sendCtx, dpEP, blob); err != nil {
		t.Fatalf("in-quota transfer should succeed: %v", err)
	}
	select {
	case got := <-received:
		if !bytes.Equal(got, blob) {
			t.Fatalf("data-plane delivered %d bytes, want %d (and equal)", len(got), len(blob))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for data-plane delivery")
	}

	// Over quota: a fresh ctl open mints a new grant; an oversized blob is rejected
	// because the quota from the 9P grant rode through to the endpoint.
	ep2, err := ns.Open(dev+"/ctl", cap)
	if err != nil {
		t.Fatalf("second open ctl: %v", err)
	}
	dpEP2 := dataplane.Endpoint{Kind: dataplane.EndpointQUIC, Addr: ep2.Endpoint, TransferID: ep2.StreamID, Cap: cap, Quota: ep2.Quota, ServerPeerID: ep2.ServerPeerID}
	over := bytes.Repeat([]byte("y"), int(q.Bytes)+1)
	overCtx, overCancel := context.WithTimeout(ctx, 5*time.Second)
	defer overCancel()
	if err := client.SendBytes(overCtx, dpEP2, over); err == nil {
		t.Fatal("over-quota transfer must be rejected")
	}
}
