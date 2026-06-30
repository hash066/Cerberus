// Package telemetry implements Vertical 08 (Observability & Tracing) of Cerberus
// — Workstream B. It publishes contract.NodeTelemetry onto the control-plane
// fabric on the Zenoh-style key cerberus/<site>/telemetry/<peer> at 1–4 Hz, and
// wraps each publish in an OpenTelemetry span for distributed tracing.
//
// Back-pressure: the publisher is lossy-newest-wins. Each tick it samples the
// current snapshot and publishes it; if the fabric cannot accept the message it
// is dropped (counted) rather than blocking, so a congested bus never stalls the
// node. This matches a telemetry stream where only the latest reading matters.
package telemetry

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	minHz = 1.0
	maxHz = 4.0
)

// Sampler returns the current node telemetry snapshot.
type Sampler func() contract.NodeTelemetry

// Config configures a telemetry Publisher.
type Config struct {
	Fabric  contract.Fabric
	Cap     contract.CapHandle
	Site    string
	PeerID  contract.PeerID
	Hz      float64        // clamped to [1,4]
	Sample  Sampler        // required
	Tracer  trace.Tracer   // required (use a no-op tracer if tracing is off)
}

// Publisher periodically publishes telemetry and records spans.
type Publisher struct {
	cfg       Config
	key       string
	interval  time.Duration
	published atomic.Uint64
	dropped   atomic.Uint64
}

// New builds a Publisher, clamping the rate into the [1,4] Hz band.
func New(cfg Config) (*Publisher, error) {
	if cfg.Fabric == nil || cfg.Sample == nil || cfg.Tracer == nil {
		return nil, fmt.Errorf("telemetry: Fabric, Sample and Tracer are required")
	}
	if cfg.Site == "" {
		cfg.Site = "local"
	}
	hz := cfg.Hz
	if hz < minHz {
		hz = minHz
	}
	if hz > maxHz {
		hz = maxHz
	}
	key := fmt.Sprintf("cerberus/%s/telemetry/%s", cfg.Site, hex.EncodeToString(cfg.PeerID[:]))
	return &Publisher{
		cfg:      cfg,
		key:      key,
		interval: time.Duration(float64(time.Second) / hz),
	}, nil
}

// Key is the fabric key this publisher writes to.
func (p *Publisher) Key() string { return p.key }

// Stats reports publish/drop counters (drops indicate back-pressure).
func (p *Publisher) Stats() (published, dropped uint64) {
	return p.published.Load(), p.dropped.Load()
}

// Run publishes until ctx is cancelled.
func (p *Publisher) Run(ctx context.Context) error {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			p.publishOnce(ctx)
		}
	}
}

func (p *Publisher) publishOnce(ctx context.Context) {
	ctx, span := p.cfg.Tracer.Start(ctx, "telemetry.publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(attribute.String("cerberus.key", p.key)),
	)
	defer span.End()

	snap := p.cfg.Sample()
	if snap.EpochMs == 0 {
		snap.EpochMs = uint64(time.Now().UnixMilli())
	}
	data, err := encode(snap)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "encode")
		p.dropped.Add(1)
		return
	}
	if err := p.cfg.Fabric.Publish(ctx, p.key, data, p.cfg.Cap); err != nil {
		// Lossy: drop the sample, never block the node on a congested bus.
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish dropped (back-pressure)")
		p.dropped.Add(1)
		return
	}
	span.SetAttributes(attribute.Int("cerberus.payload_bytes", len(data)))
	p.published.Add(1)
}
