package telemetry

import (
	"context"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestPublisherEmitsTelemetryAndSpans(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	fab := stub.NewFabric()
	kernel := stub.NewCapKernel()
	capH, _ := kernel.Mint(
		contract.ResourceRef{Kind: contract.KindTopic, Path: "cerberus/local/telemetry/**"},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)

	sub, err := fab.Subscribe(ctx, "cerberus/local/telemetry/**", capH)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	tracer := tp.Tracer("telemetry-test")

	var pid contract.PeerID
	pid[0] = 0xAB
	sampler := func() contract.NodeTelemetry {
		return contract.NodeTelemetry{
			PeerID: pid,
			Memory: contract.Memory{RAMFree: 1234},
		}
	}

	pub, err := New(Config{
		Fabric: fab, Cap: capH, Site: "local", PeerID: pid,
		Hz: 4, Sample: sampler, Tracer: tracer,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	go func() { _ = pub.Run(ctx) }()

	select {
	case s := <-sub:
		tel, err := Decode(s.Payload)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if tel.Memory.RAMFree != 1234 {
			t.Fatalf("RAMFree = %d, want 1234", tel.Memory.RAMFree)
		}
		if tel.EpochMs == 0 {
			t.Fatal("EpochMs not stamped")
		}
	case <-ctx.Done():
		t.Fatal("no telemetry received within deadline")
	}

	// Allow a couple more ticks, then verify spans were recorded.
	time.Sleep(300 * time.Millisecond)
	cancel()

	if pubd, _ := pub.Stats(); pubd == 0 {
		t.Fatal("published counter is zero")
	}
	var found bool
	for _, sp := range sr.Ended() {
		if sp.Name() == "telemetry.publish" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no telemetry.publish span recorded")
	}
}

// capFabric is a fabric fake that actually enforces the topic capability via a
// CapKernel before accepting a publish. It proves telemetry flows through the
// cap-gated Publish path (the seam the real mesh.Fabric implements), so a missing
// or revoked cap is denied rather than silently published.
type capFabric struct {
	kernel contract.CapKernel
	got    chan contract.Sample
}

func (c *capFabric) Publish(_ context.Context, key string, msg []byte, capH contract.CapHandle) error {
	req := contract.Request{Op: "publish", Resource: contract.ResourceRef{Kind: contract.KindTopic, Path: key}}
	if err := c.kernel.Verify(capH, req, time.Now().Unix()); err != nil {
		return err
	}
	select {
	case c.got <- contract.Sample{Key: key, Payload: msg}:
	default:
	}
	return nil
}

func (c *capFabric) Subscribe(context.Context, string, contract.CapHandle) (<-chan contract.Sample, error) {
	return nil, nil
}
func (c *capFabric) Dial(contract.PeerID) (contract.Session, error) { return nil, nil }
func (c *capFabric) Peers() []contract.PeerInfo                     { return nil }

// TestTelemetryGoesThroughCapGate verifies that telemetry is published only with
// a valid topic capability: with a valid cap a sample is delivered; with no cap
// the publisher's publish is denied and the sample is dropped (back-pressure
// counter increments), never reaching the bus.
func TestTelemetryGoesThroughCapGate(t *testing.T) {
	kernel := stub.NewCapKernel()
	var pid contract.PeerID
	pid[0] = 0x7E
	sampler := func() contract.NodeTelemetry { return contract.NodeTelemetry{PeerID: pid} }
	tracer := sdktrace.NewTracerProvider().Tracer("cap-gate-test")

	// (1) Valid cap: sample is delivered.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		fab := &capFabric{kernel: kernel, got: make(chan contract.Sample, 4)}
		capH, _ := kernel.Mint(
			contract.ResourceRef{Kind: contract.KindTopic, Path: "cerberus/local/telemetry/**"},
			[]contract.Right{contract.RightRead, contract.RightWrite}, nil)
		pub, err := New(Config{Fabric: fab, Cap: capH, Site: "local", PeerID: pid, Hz: 4, Sample: sampler, Tracer: tracer})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		go func() { _ = pub.Run(ctx) }()
		select {
		case <-fab.got:
			// delivered through the cap gate — good.
		case <-ctx.Done():
			t.Fatal("telemetry not delivered with a valid cap")
		}
	}

	// (2) No cap (zero handle is unknown to the kernel): every publish is denied,
	// so nothing reaches the bus and the publisher counts drops.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		fab := &capFabric{kernel: kernel, got: make(chan contract.Sample, 4)}
		pub, err := New(Config{Fabric: fab, Cap: contract.CapHandle(0), Site: "local", PeerID: pid, Hz: 4, Sample: sampler, Tracer: tracer})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		go func() { _ = pub.Run(ctx) }()
		<-ctx.Done()
		if _, dropped := pub.Stats(); dropped == 0 {
			t.Fatal("publish without a cap was not denied/dropped")
		}
		select {
		case s := <-fab.got:
			t.Fatalf("uncapped telemetry reached the bus: %q", s.Key)
		default:
		}
	}
}

func TestRateClamped(t *testing.T) {
	mk := func(hz float64) *Publisher {
		p, err := New(Config{
			Fabric: stub.NewFabric(), Site: "local",
			Hz: hz, Sample: func() contract.NodeTelemetry { return contract.NodeTelemetry{} },
			Tracer: sdktrace.NewTracerProvider().Tracer("x"),
		})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		return p
	}
	if d := mk(100).interval; d < 250*time.Millisecond {
		t.Fatalf("rate not clamped to 4Hz: interval=%v", d)
	}
	if d := mk(0.1).interval; d > time.Second {
		t.Fatalf("rate not clamped to 1Hz: interval=%v", d)
	}
}
