package metrics_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/metrics"
)

// lineFor returns the value line for a metric name from exposition text, or "".
func lineFor(text, name string) string {
	for _, ln := range strings.Split(text, "\n") {
		if strings.HasPrefix(ln, name+" ") {
			return ln
		}
	}
	return ""
}

// TestRegistryRendersCountersAndGauges proves registered metrics appear in the
// /metrics text with the correct values after increments, including HELP/TYPE
// lines and gauge formatting.
func TestRegistryRendersCountersAndGauges(t *testing.T) {
	r := metrics.NewRegistry()
	placed := r.Counter("cerberus_tasks_placed_total", "Tasks placed.")
	bytes := r.Counter("cerberus_bytes_transferred_total", "Bytes moved.")
	peers := r.Gauge("cerberus_peers", "Connected peers.")

	placed.Inc()
	placed.Inc()
	placed.Add(3) // -> 5
	bytes.Add(4096)
	peers.Set(2)
	peers.Inc() // -> 3

	out := r.Render()

	if got := lineFor(out, "cerberus_tasks_placed_total"); got != "cerberus_tasks_placed_total 5" {
		t.Fatalf("counter value wrong: %q", got)
	}
	if got := lineFor(out, "cerberus_bytes_transferred_total"); got != "cerberus_bytes_transferred_total 4096" {
		t.Fatalf("counter value wrong: %q", got)
	}
	if got := lineFor(out, "cerberus_peers"); got != "cerberus_peers 3" {
		t.Fatalf("gauge value wrong: %q", got)
	}
	if !strings.Contains(out, "# HELP cerberus_tasks_placed_total Tasks placed.") {
		t.Fatalf("missing HELP line:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE cerberus_tasks_placed_total counter") {
		t.Fatalf("missing TYPE counter line:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE cerberus_peers gauge") {
		t.Fatalf("missing TYPE gauge line:\n%s", out)
	}
}

// TestRegistrySameNameSharesInstance proves re-registering a name returns the
// same handle (so multiple call sites accumulate into one metric).
func TestRegistrySameNameSharesInstance(t *testing.T) {
	r := metrics.NewRegistry()
	a := r.Counter("cerberus_x_total", "x")
	b := r.Counter("cerberus_x_total", "x")
	a.Inc()
	b.Inc()
	if a.Value() != 2 || b.Value() != 2 {
		t.Fatalf("shared counter not shared: a=%d b=%d", a.Value(), b.Value())
	}
}

// TestNewMetricsRegistersFullSet proves the concrete daemon metric set renders
// and that a typed increment lands in the text output.
func TestNewMetricsRegistersFullSet(t *testing.T) {
	m := metrics.NewMetrics()
	m.TasksPlaced.Inc()
	m.BytesTransferred.Add(1024)
	m.Peers.Set(4)
	m.RevocationsTotal.Inc()

	out := m.Registry.Render()
	for _, want := range []string{
		"cerberus_tasks_placed_total 1",
		"cerberus_bytes_transferred_total 1024",
		"cerberus_peers 4",
		"cerberus_revocations_total 1",
		"cerberus_gateway_requests_total 0",
		"cerberus_build_info 1",
	} {
		name := strings.Fields(want)[0]
		if got := lineFor(out, name); got != want {
			t.Fatalf("want %q, got %q\nfull:\n%s", want, got, out)
		}
	}
}

func TestHealthzAlwaysOK(t *testing.T) {
	srv := metrics.New(metrics.NewRegistry(), nil, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("healthz should be 200, got %d", w.Result().StatusCode)
	}
	if body := w.Body.String(); body != "ok" {
		t.Fatalf("healthz body = %q", body)
	}
}

// TestReadyzReflectsReadiness proves /readyz returns 503 while not ready and 200
// once the readiness predicate flips.
func TestReadyzReflectsReadiness(t *testing.T) {
	ready := false
	reason := "mesh starting"
	srv := metrics.New(metrics.NewRegistry(), nil, func() (bool, string) {
		return ready, reason
	})

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Result().StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz should be 503 while not ready, got %d", w.Result().StatusCode)
	}
	if !strings.Contains(w.Body.String(), "mesh starting") {
		t.Fatalf("readyz body missing reason: %q", w.Body.String())
	}

	ready = true
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("readyz should be 200 once ready, got %d", w.Result().StatusCode)
	}
}

func TestReadyzNilFuncAlwaysReady(t *testing.T) {
	srv := metrics.New(metrics.NewRegistry(), nil, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("readyz with nil predicate should be 200, got %d", w.Result().StatusCode)
	}
}

// TestMetricsEndpointServesText proves the /metrics endpoint returns the
// exposition text with the Prometheus content type when no auth is configured.
func TestMetricsEndpointServesText(t *testing.T) {
	m := metrics.NewMetrics()
	m.GatewayRequests.Add(7)
	srv := metrics.New(m.Registry, nil, nil)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("metrics should be 200, got %d", w.Result().StatusCode)
	}
	if ct := w.Header().Get("Content-Type"); ct != metrics.ContentType {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), "cerberus_gateway_requests_total 7") {
		t.Fatalf("metrics body missing counter:\n%s", w.Body.String())
	}
}

// TestMetricsEndpointAuthGated proves that when an Authorizer is configured,
// /metrics requires a Bearer token granting "read" (matching daemon/api), while
// /healthz stays open.
func TestMetricsEndpointAuthGated(t *testing.T) {
	iss, err := auth.NewIssuer()
	if err != nil {
		t.Fatal(err)
	}
	srv := metrics.New(metrics.NewMetrics().Registry, iss, nil)

	// No token -> 401.
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("metrics without token should be 401, got %d", w.Result().StatusCode)
	}

	// healthz stays open even with auth configured.
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("healthz should stay open, got %d", w.Result().StatusCode)
	}

	// Valid read token -> 200.
	tok, err := iss.Mint("operator", []string{"read"}, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("metrics with valid token should be 200, got %d", w.Result().StatusCode)
	}
}

// TestServeOverListener covers the Serve(net.Listener) variant cerberusd's
// main() now uses: the caller binds the listener itself first (so a bind
// conflict on the configured address is visible and can fall back to an
// ephemeral port), then hands the listener to Serve instead of letting
// Start's internal ListenAndServe swallow the bind step silently.
func TestServeOverListener(t *testing.T) {
	srv := metrics.New(metrics.NewMetrics().Registry, nil, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	defer ln.Close()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz over bound listener: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz over Serve(ln) should be 200, got %d", resp.StatusCode)
	}
}
