// Package metrics is Cerberus's local metrics + health surface (Vertical 08,
// Observability). It exposes a /metrics endpoint in Prometheus text exposition
// format plus liveness (/healthz) and readiness (/readyz) probes.
//
// Why hand-rolled: the daemon already pulls in the OTel SDK for tracing; adding
// the full Prometheus client + collector machinery for a handful of process
// counters is weight we don't need at v0.1. The registry here is a few hundred
// lines of std-lib-only code, deterministically ordered, and emits text that any
// Prometheus scraper (or `curl`) reads. When a deployment wants the full client
// (histograms, exemplars, pushgateway) it can be swapped behind this same
// Server seam without touching call sites.
//
// Concurrency: counters and gauges are atomic; the registry is guarded by a
// RWMutex so registration (startup) and scraping (concurrent HTTP) are safe.
package metrics

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Counter is a monotonically increasing value (resets only on restart).
type Counter struct{ v atomic.Uint64 }

// Inc adds 1.
func (c *Counter) Inc() { c.v.Add(1) }

// Add adds delta (delta is uint64; counters never decrease).
func (c *Counter) Add(delta uint64) { c.v.Add(delta) }

// Value returns the current count.
func (c *Counter) Value() uint64 { return c.v.Load() }

// Gauge is a value that can go up or down (e.g. connected peers). Stored as the
// bit pattern of a float64 so fractional gauges (load, temperature) work too.
type Gauge struct{ bits atomic.Uint64 }

// Set sets the gauge to v.
func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }

// Add adds delta (may be negative).
func (g *Gauge) Add(delta float64) {
	for {
		old := g.bits.Load()
		nv := math.Float64frombits(old) + delta
		if g.bits.CompareAndSwap(old, math.Float64bits(nv)) {
			return
		}
	}
}

// Inc/Dec are conveniences for counting things that come and go.
func (g *Gauge) Inc() { g.Add(1) }
func (g *Gauge) Dec() { g.Add(-1) }

// Value returns the current gauge value.
func (g *Gauge) Value() float64 { return math.Float64frombits(g.bits.Load()) }

type metricKind int

const (
	kindCounter metricKind = iota
	kindGauge
)

type metric struct {
	name    string
	help    string
	kind    metricKind
	counter *Counter
	gauge   *Gauge
}

// Registry holds the daemon's metrics and renders them in Prometheus text
// exposition format. Construct with NewRegistry; register metrics at startup.
type Registry struct {
	mu      sync.RWMutex
	order   []string // registration order, for stable output grouping
	metrics map[string]*metric
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{metrics: map[string]*metric{}}
}

// Counter registers (or returns the existing) counter named name. Re-registering
// the same name returns the original instance so multiple call sites share it.
// Metric names must be valid Prometheus identifiers ([a-zA-Z_:][a-zA-Z0-9_:]*).
func (r *Registry) Counter(name, help string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.metrics[name]; ok {
		if m.kind != kindCounter {
			panic("metrics: " + name + " already registered as a non-counter")
		}
		return m.counter
	}
	mustValidName(name)
	c := &Counter{}
	r.metrics[name] = &metric{name: name, help: help, kind: kindCounter, counter: c}
	r.order = append(r.order, name)
	return c
}

// Gauge registers (or returns the existing) gauge named name.
func (r *Registry) Gauge(name, help string) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.metrics[name]; ok {
		if m.kind != kindGauge {
			panic("metrics: " + name + " already registered as a non-gauge")
		}
		return m.gauge
	}
	mustValidName(name)
	g := &Gauge{}
	r.metrics[name] = &metric{name: name, help: help, kind: kindGauge, gauge: g}
	r.order = append(r.order, name)
	return g
}

// Write renders all metrics in Prometheus text exposition format (v0.0.4):
// a HELP line, a TYPE line, then the value line, per metric. Metric blocks are
// emitted in registration order for a stable, diff-friendly scrape.
func (r *Registry) Write(w *strings.Builder) {
	r.mu.RLock()
	names := make([]string, len(r.order))
	copy(names, r.order)
	ms := make(map[string]*metric, len(r.metrics))
	for k, v := range r.metrics {
		ms[k] = v
	}
	r.mu.RUnlock()

	for _, name := range names {
		m := ms[name]
		if m.help != "" {
			fmt.Fprintf(w, "# HELP %s %s\n", m.name, escapeHelp(m.help))
		}
		switch m.kind {
		case kindCounter:
			fmt.Fprintf(w, "# TYPE %s counter\n", m.name)
			fmt.Fprintf(w, "%s %d\n", m.name, m.counter.Value())
		case kindGauge:
			fmt.Fprintf(w, "# TYPE %s gauge\n", m.name)
			fmt.Fprintf(w, "%s %s\n", m.name, formatFloat(m.gauge.Value()))
		}
	}
}

// Render returns the exposition text as a string (convenience for tests/handlers).
func (r *Registry) Render() string {
	var b strings.Builder
	r.Write(&b)
	return b.String()
}

// Names returns the registered metric names, sorted (for tests/introspection).
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.metrics))
	for n := range r.metrics {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// formatFloat renders a gauge value the way Prometheus expects: integers without
// a decimal point, plus the special +Inf/-Inf/NaN tokens.
func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	case f == math.Trunc(f) && math.Abs(f) < 1e15:
		return strconv.FormatInt(int64(f), 10)
	default:
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
}

func escapeHelp(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

func mustValidName(name string) {
	if name == "" {
		panic("metrics: empty metric name")
	}
	for i, r := range name {
		ok := r == '_' || r == ':' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(i > 0 && r >= '0' && r <= '9')
		if !ok {
			panic("metrics: invalid metric name " + strconv.Quote(name))
		}
	}
}
