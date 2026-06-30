package mesh

import (
	"context"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

func TestMatchKey(t *testing.T) {
	cases := []struct {
		expr, key string
		want      bool
	}{
		{"cerberus/site-a/telemetry/n1", "cerberus/site-a/telemetry/n1", true},
		{"cerberus/site-a/telemetry/*", "cerberus/site-a/telemetry/n1", true},
		{"cerberus/site-a/telemetry/*", "cerberus/site-a/telemetry/n1/extra", false},
		{"cerberus/site-a/**", "cerberus/site-a/telemetry/n1", true},
		{"cerberus/**/n1", "cerberus/site-a/telemetry/n1", true},
		{"cerberus/**", "cerberus", true},
		{"cerberus/site-a/crdt/*", "cerberus/site-a/telemetry/n1", false},
		{"cerberus/site-a/telemetry/n1", "cerberus/site-b/telemetry/n1", false},
	}
	for _, c := range cases {
		if got := MatchKey(c.expr, c.key); got != c.want {
			t.Errorf("MatchKey(%q,%q)=%v want %v", c.expr, c.key, got, c.want)
		}
	}
}

// TestTwoNodeExchange spins up two in-process libp2p/QUIC fabric nodes, connects
// them, and verifies (a) capability-gated Gossipsub pub/sub delivery across the
// mesh and (b) a point-to-point QUIC Session round-trip.
func TestTwoNodeExchange(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	kA := stub.NewCapKernel()
	kB := stub.NewCapKernel()

	a, err := New(ctx, Config{Site: "test", Kernel: kA})
	if err != nil {
		t.Fatalf("node A: %v", err)
	}
	defer a.Close()
	b, err := New(ctx, Config{Site: "test", Kernel: kB})
	if err != nil {
		t.Fatalf("node B: %v", err)
	}
	defer b.Close()

	if err := a.Connect(ctx, b.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// --- (a) pub/sub exchange over the site bus ---
	capSub, _ := kB.Mint(contract.ResourceRef{Kind: contract.KindTopic}, nil, nil)
	ch, err := b.Subscribe(ctx, "cerberus/test/telemetry/**", capSub)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	capPub, _ := kA.Mint(contract.ResourceRef{Kind: contract.KindTopic}, nil, nil)
	want := []byte("hello-fabric")

	// Gossipsub needs the subscription to propagate; publish on a ticker until
	// the message is delivered or we time out.
	got := publishUntilReceived(t, ctx, a, ch, "cerberus/test/telemetry/n1", want, capPub)
	if string(got) != string(want) {
		t.Fatalf("pub/sub payload = %q want %q", got, want)
	}

	// --- (b) QUIC Session round-trip A -> B ---
	go func() {
		s, err := a.Dial(b.PeerID())
		if err != nil {
			return
		}
		_ = s.Send([]byte("ping"))
	}()

	select {
	case sess := <-b.Inbound():
		msg, err := sess.Recv()
		if err != nil {
			t.Fatalf("session recv: %v", err)
		}
		if string(msg) != "ping" {
			t.Fatalf("session payload = %q want %q", msg, "ping")
		}
		_ = sess.Close()
	case <-ctx.Done():
		t.Fatal("timed out waiting for inbound session")
	}

	if len(a.Peers()) == 0 {
		t.Fatal("node A reports no peers after connect")
	}
}

// TestDialBindsAndRejectsPeerIDMismatch verifies the PeerID-binding enforcement
// over the real libp2p/QUIC handshake:
//   - dialing a peer by its true PeerID succeeds and the returned session is
//     cryptographically bound to that authenticated identity;
//   - dialing while claiming a *different* PeerID for the same address is
//     rejected (the authenticated key cannot match a forged claim).
func TestDialBindsAndRejectsPeerIDMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node A: %v", err)
	}
	defer a.Close()
	b, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node B: %v", err)
	}
	defer b.Close()

	if err := a.Connect(ctx, b.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// (1) Dial B by its real PeerID: must succeed and bind to B's identity.
	sess, err := a.Dial(b.PeerID())
	if err != nil {
		t.Fatalf("dial valid PeerID: %v", err)
	}
	ss, ok := sess.(*streamSession)
	if !ok {
		t.Fatalf("unexpected session type %T", sess)
	}
	rid, verified := ss.RemotePeerID()
	if !verified || rid != b.PeerID() {
		t.Fatalf("session not bound to B: verified=%v rid=%x want=%x", verified, rid, b.PeerID())
	}
	_ = sess.Close()

	// (2) Dial the same address while claiming a PeerID we do NOT hold the key
	// for. libp2p resolves the stream by the claimed peer.ID, so there is no
	// route to an impostor and the dial fails — a session bound to a key that
	// isn't the claimed PeerID is never returned.
	_, forged := newPeer(t)
	if _, err := a.Dial(forged); err == nil {
		t.Fatal("dial with a forged claimed PeerID unexpectedly succeeded")
	}
}

// TestPublishSubscribeCapGate proves topics are capability-gated: publish and
// subscribe succeed with a valid topic cap, and are denied without one.
func TestPublishSubscribeCapGate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	k := stub.NewCapKernel()
	f, err := New(ctx, Config{Site: "test", Kernel: k})
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	defer f.Close()

	// No capability at all (zero handle is unknown to the kernel) -> DENIED.
	if _, err := f.Subscribe(ctx, "cerberus/test/telemetry/**", contract.CapHandle(0)); err == nil {
		t.Fatal("subscribe without a cap was allowed")
	}
	if err := f.Publish(ctx, "cerberus/test/telemetry/n1", []byte("x"), contract.CapHandle(0)); err == nil {
		t.Fatal("publish without a cap was allowed")
	}

	// Valid topic cap -> allowed.
	capH, _ := k.Mint(contract.ResourceRef{Kind: contract.KindTopic}, nil, nil)
	if _, err := f.Subscribe(ctx, "cerberus/test/telemetry/**", capH); err != nil {
		t.Fatalf("subscribe with valid cap denied: %v", err)
	}
	if err := f.Publish(ctx, "cerberus/test/telemetry/n1", []byte("x"), capH); err != nil {
		t.Fatalf("publish with valid cap denied: %v", err)
	}

	// Revoked cap -> denied again (REVOKED).
	if err := k.Revoke(capH); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := f.Publish(ctx, "cerberus/test/telemetry/n1", []byte("x"), capH); err == nil {
		t.Fatal("publish with a revoked cap was allowed")
	}
}

func publishUntilReceived(t *testing.T, ctx context.Context, pub *Fabric, ch <-chan contract.Sample, key string, payload []byte, capH contract.CapHandle) []byte {
	t.Helper()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := pub.Publish(ctx, key, payload, capH); err != nil {
			t.Fatalf("publish: %v", err)
		}
		select {
		case s := <-ch:
			return s.Payload
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("timed out waiting for pub/sub delivery")
			return nil
		}
	}
}
