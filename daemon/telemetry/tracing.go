package telemetry

// tracing.go turns the telemetry package's spans into real, exported traces.
// Until now the daemon handed the Publisher a no-op tracer (spans created, never
// recorded). This builds a real OpenTelemetry SDK TracerProvider so the
// telemetry.publish spans (and any other cerberusd spans) are actually sampled,
// batched, and exported.
//
// The default exporter is intentionally dependency-free: a concise one-line-per-
// span log writer (name, duration, status), so a developer can see the trace
// stream without standing up an OTLP collector. Swapping in an OTLP/Jaeger
// exporter is a one-line change at the WithBatcher call.

import (
	"context"
	"log"
	"os"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerName is the instrumentation scope name for cerberusd spans.
const TracerName = "cerberusd"

// NewTracerProvider returns a tracer and a shutdown func. When export is true it
// builds a real SDK provider that batches finished spans to a concise log
// exporter; the shutdown func flushes and stops it (call it on daemon exit).
// When export is false it returns a no-op tracer (zero overhead) and a no-op
// shutdown, so tracing is strictly opt-in.
func NewTracerProvider(export bool) (trace.Tracer, func(context.Context) error) {
	if !export {
		return noop.NewTracerProvider().Tracer(TracerName), func(context.Context) error { return nil }
	}
	exp := newLogExporter(log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds))
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	return tp.Tracer(TracerName), tp.Shutdown
}

// logExporter is a minimal sdktrace.SpanExporter that writes one concise line per
// finished span. It is the zero-infrastructure default; real deployments point
// WithBatcher at an OTLP exporter instead.
type logExporter struct{ log *log.Logger }

func newLogExporter(l *log.Logger) *logExporter { return &logExporter{log: l} }

// ExportSpans implements sdktrace.SpanExporter.
func (e *logExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, s := range spans {
		if err := ctx.Err(); err != nil {
			return err
		}
		dur := s.EndTime().Sub(s.StartTime())
		e.log.Printf("trace span=%q dur=%s status=%s attrs=%d",
			s.Name(), dur, s.Status().Code, len(s.Attributes()))
	}
	return nil
}

// Shutdown implements sdktrace.SpanExporter.
func (e *logExporter) Shutdown(context.Context) error { return nil }
