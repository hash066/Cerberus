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
	capH, _ := kernel.Mint(contract.ResourceRef{Kind: contract.KindTopic}, nil, nil)

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
