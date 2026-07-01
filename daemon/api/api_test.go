package api_test

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hash066/cerberus/daemon/api"
	"github.com/hash066/cerberus/daemon/auth"
)

func snap() api.Snapshot {
	return api.Snapshot{
		Version: "0.1.0", Profile: "open_mesh", Kernel: "pure-go-stub",
		UptimeSec: 5, MeshUp: true, Peers: []string{"peerA"}, OperatorBalance: 1000,
		Power: api.PowerView{Source: "AC", BatteryPct: 100, Lid: "open", Hint: "awake"},
	}
}

func TestHealthzNoAuth(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.New(iss, snap)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("healthz should be 200, got %d", w.Result().StatusCode)
	}
}

func TestStatusRequiresToken(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.New(iss, snap)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without token should be 401, got %d", w.Result().StatusCode)
	}
}

func TestStatusWithValidToken(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.New(iss, snap)
	tok, _ := iss.Mint("operator", []string{"read"}, "", time.Hour)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}
	var got api.Snapshot
	if err := json.NewDecoder(w.Result().Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.1.0" || got.OperatorBalance != 1000 || !got.MeshUp {
		t.Fatalf("unexpected snapshot: %+v", got)
	}
}

// TestServeOverListener covers the Serve(net.Listener) variant cerberusd's
// main() now uses: it binds the listener itself (so it can react to a
// conflict/fall back to an ephemeral port), then hands it to Serve instead of
// letting Start's internal ListenAndServe swallow the bind step. This checks
// Serve actually answers HTTP requests on a listener bound out-of-band.
func TestServeOverListener(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.New(iss, snap)

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
