package telemetry

import (
	"context"
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestPublisherSpansAreExported proves the publisher's spans flow through a real
// SDK pipeline to an exporter (not a no-op): one telemetry.publish span lands in
// the in-memory exporter for each publishOnce.
func TestPublisherSpansAreExported(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	p, err := New(Config{
		Fabric: stub.NewFabric(),
		Cap:    1,
		Site:   "test",
		Sample: func() contract.NodeTelemetry { return contract.NodeTelemetry{} },
		Tracer: tp.Tracer("test"),
	})
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	p.publishOnce(context.Background())

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly one exported span, got %d", len(spans))
	}
	if spans[0].Name != "telemetry.publish" {
		t.Fatalf("unexpected span name %q", spans[0].Name)
	}
}

// TestNewTracerProviderDisabledIsNoop confirms tracing is zero-cost and safe when
// disabled: a span can be started/ended and shutdown returns nil.
func TestNewTracerProviderDisabledIsNoop(t *testing.T) {
	tr, shutdown := NewTracerProvider(false)
	_, span := tr.Start(context.Background(), "x")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("noop shutdown should be nil: %v", err)
	}
}

// TestNewTracerProviderEnabledFlushes confirms the enabled path builds a real
// provider whose shutdown flushes cleanly.
func TestNewTracerProviderEnabledFlushes(t *testing.T) {
	tr, shutdown := NewTracerProvider(true)
	_, span := tr.Start(context.Background(), "y")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown/flush: %v", err)
	}
}
