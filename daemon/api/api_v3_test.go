package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hash066/cerberus/daemon/api"
	"github.com/hash066/cerberus/daemon/auth"
)

// richSnap exercises every additive field so the serialization test proves they
// round-trip over the wire.
func richSnap() api.Snapshot {
	return api.Snapshot{
		Version: "0.1.0", Profile: "open_mesh", Kernel: "pure-go-stub",
		UptimeSec: 5, MeshUp: true, Peers: []string{"peerA"}, OperatorBalance: 1000,
		Power: api.PowerView{Source: "AC", BatteryPct: 100, Lid: "open", Hint: "awake"},
		Devices: []api.Device{
			{ID: "self", Kind: "self", Online: true},
			{ID: "peerA", Addr: "1.2.3.4:9000", Kind: "peer", Online: true, LastSeen: 42},
		},
		Wallet:          api.Wallet{Principal: "operator", Balance: 1000, Locked: 50, Pending: 1},
		BeliefConflicts: 3,
		Metrics: api.MetricsSummary{
			TasksPlaced: 12, Transfers: 4, BytesMoved: 8192, TasksExecuted: 11, Errors: 1,
		},
		Subsystems: []api.Health{
			{Name: "mesh", Status: "ok"},
			{Name: "scheduler", Status: "degraded", Detail: "1 node down"},
		},
	}
}

func TestStatusSerializesV3Fields(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.New(iss, richSnap)
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
	// Legacy fields intact.
	if got.Version != "0.1.0" || got.OperatorBalance != 1000 || !got.MeshUp || len(got.Peers) != 1 {
		t.Fatalf("legacy fields wrong: %+v", got)
	}
	// New fields round-tripped.
	if len(got.Devices) != 2 || got.Devices[1].Addr != "1.2.3.4:9000" || !got.Devices[1].Online {
		t.Fatalf("devices wrong: %+v", got.Devices)
	}
	if got.Wallet.Balance != 1000 || got.Wallet.Locked != 50 || got.Wallet.Principal != "operator" {
		t.Fatalf("wallet wrong: %+v", got.Wallet)
	}
	if got.BeliefConflicts != 3 {
		t.Fatalf("belief conflicts wrong: %d", got.BeliefConflicts)
	}
	if got.Metrics.TasksPlaced != 12 || got.Metrics.BytesMoved != 8192 || got.Metrics.Errors != 1 {
		t.Fatalf("metrics wrong: %+v", got.Metrics)
	}
	if len(got.Subsystems) != 2 || got.Subsystems[1].Status != "degraded" {
		t.Fatalf("subsystems wrong: %+v", got.Subsystems)
	}
}

// TestSnapshotJSONKeysStable pins the wire keys so the tray contract doesn't
// drift silently.
func TestSnapshotJSONKeysStable(t *testing.T) {
	b, err := json.Marshal(richSnap())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"version", "profile", "kernel", "uptime_sec", "mesh_up", "peers",
		"operator_balance", "power", "devices", "wallet", "belief_conflicts",
		"metrics", "subsystems",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing expected JSON key %q", k)
		}
	}
}

// --- Getters seam ------------------------------------------------------------

func TestGettersAssembleSnapshot(t *testing.T) {
	g := api.Getters{
		Version:         func() string { return "9.9.9" },
		Profile:         func() string { return "sealed" },
		MeshUp:          func() bool { return true },
		Peers:           func() []string { return []string{"p1", "p2"} },
		BeliefConflicts: func() int { return 7 },
		Metrics:         func() api.MetricsSummary { return api.MetricsSummary{TasksPlaced: 3} },
		Wallet:          func() api.Wallet { return api.Wallet{Principal: "operator", Balance: 500} },
	}
	s := g.Snapshot()
	if s.Version != "9.9.9" || s.Profile != "sealed" || !s.MeshUp {
		t.Fatalf("scalar getters wrong: %+v", s)
	}
	if len(s.Peers) != 2 || s.BeliefConflicts != 7 || s.Metrics.TasksPlaced != 3 {
		t.Fatalf("collection getters wrong: %+v", s)
	}
	// Wallet balance mirrors into the legacy OperatorBalance.
	if s.OperatorBalance != 500 {
		t.Fatalf("expected operator balance mirrored from wallet, got %d", s.OperatorBalance)
	}
}

func TestGettersNilFieldsAreZero(t *testing.T) {
	// An empty Getters must still yield a valid, zero-ish Snapshot (partial wiring).
	s := api.Getters{}.Snapshot()
	if s.Version != "" || s.MeshUp || len(s.Peers) != 0 || s.BeliefConflicts != 0 {
		t.Fatalf("expected zero snapshot, got %+v", s)
	}
	// Principal defaulted even with no wallet getter.
	if s.Wallet.Principal != "operator" {
		t.Fatalf("expected default principal, got %q", s.Wallet.Principal)
	}
}

func TestGettersLegacyBalanceMirrorsToWallet(t *testing.T) {
	g := api.Getters{OperatorBalance: func() uint64 { return 250 }}
	s := g.Snapshot()
	if s.Wallet.Balance != 250 {
		t.Fatalf("expected legacy balance mirrored into wallet, got %d", s.Wallet.Balance)
	}
}

func TestNewWithGettersServesStatus(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{
		Version: func() string { return "1.0.0" },
		Devices: func() []api.Device { return []api.Device{{ID: "self", Online: true}} },
	})
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
	if got.Version != "1.0.0" || len(got.Devices) != 1 {
		t.Fatalf("unexpected snapshot from getters: %+v", got)
	}
}

// NewWithGetters must still enforce the read capability (no auth regression).
func TestNewWithGettersRequiresToken(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w.Result().StatusCode)
	}
}
