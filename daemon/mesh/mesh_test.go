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
