package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hash066/cerberus/daemon/api"
	"github.com/hash066/cerberus/daemon/auth"
)

func readToken(t *testing.T, iss *auth.Issuer, rights ...string) string {
	t.Helper()
	tok, err := iss.Mint("operator", rights, "", time.Hour)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return tok
}

func doJSON(t *testing.T, srv *api.Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// --- GET /api/v1/conflicts ---------------------------------------------------

func TestConflictsListRequiresToken(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{})
	w := doJSON(t, srv, http.MethodGet, "/api/v1/conflicts", "", nil)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Result().StatusCode)
	}
}

func TestConflictsListReturnsCandidatesShape(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{}).WithListGetters(api.ListGetters{
		Conflicts: func() []api.ConflictView {
			return []api.ConflictView{
				{Subject: "belief:foo", Candidates: []api.ConflictCandidateView{
					{Actor: "peerA", Value: "1"},
					{Actor: "peerB", Value: "2"},
				}},
			}
		},
	})
	tok := readToken(t, iss, "read")
	w := doJSON(t, srv, http.MethodGet, "/api/v1/conflicts", tok, nil)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(got))
	}
	if got[0]["subject"] != "belief:foo" {
		t.Fatalf("unexpected subject: %+v", got[0])
	}
	cands, ok := got[0]["candidates"].([]any)
	if !ok || len(cands) != 2 {
		t.Fatalf("expected 2 candidates under 'candidates' key, got %+v", got[0])
	}
}

func TestConflictsListEmptyIsEmptyArrayNotNull(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{})
	tok := readToken(t, iss, "read")
	w := doJSON(t, srv, http.MethodGet, "/api/v1/conflicts", tok, nil)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}
	if got := w.Body.String(); got != "[]\n" && got != "[]" {
		t.Fatalf("expected empty JSON array body, got %q", got)
	}
}

// --- POST /api/v1/conflicts/resolve -----------------------------------------

func TestConflictsResolveRequiresWriteToken(t *testing.T) {
	iss, _ := auth.NewIssuer()
	called := false
	srv := api.NewWithGetters(iss, api.Getters{}).WithActions(api.Actions{
		ResolveConflict: func(subject, winning string) (bool, error) {
			called = true
			return true, nil
		},
	})
	// No token at all.
	w := doJSON(t, srv, http.MethodPost, "/api/v1/conflicts/resolve", "", api.ResolveConflictRequest{Subject: "s", Winning: "w"})
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d", w.Result().StatusCode)
	}
	// read-only token must not satisfy a write-gated route.
	readTok := readToken(t, iss, "read")
	w = doJSON(t, srv, http.MethodPost, "/api/v1/conflicts/resolve", readTok, api.ResolveConflictRequest{Subject: "s", Winning: "w"})
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with read-only token, got %d", w.Result().StatusCode)
	}
	if called {
		t.Fatal("ResolveConflict must not be invoked when auth fails")
	}
}

func TestConflictsResolveWithWriteTokenSucceeds(t *testing.T) {
	iss, _ := auth.NewIssuer()
	var gotSubject, gotWinning string
	srv := api.NewWithGetters(iss, api.Getters{}).WithActions(api.Actions{
		ResolveConflict: func(subject, winning string) (bool, error) {
			gotSubject, gotWinning = subject, winning
			return true, nil
		},
	})
	tok := readToken(t, iss, "write")
	w := doJSON(t, srv, http.MethodPost, "/api/v1/conflicts/resolve", tok, api.ResolveConflictRequest{Subject: "belief:x", Winning: "42"})
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	if gotSubject != "belief:x" || gotWinning != "42" {
		t.Fatalf("action not invoked with expected args: subject=%q winning=%q", gotSubject, gotWinning)
	}
	var resp api.ResolveConflictResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Resolved || resp.Subject != "belief:x" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// Admin (operator) tokens carry "admin", which auth.Claims.allows treats as
// satisfying any right — so the operator token (the tray's only credential)
// must still work against the write-gated route.
func TestConflictsResolveAdminTokenSatisfiesWriteGate(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{}).WithActions(api.Actions{
		ResolveConflict: func(subject, winning string) (bool, error) { return true, nil },
	})
	tok := readToken(t, iss, "admin")
	w := doJSON(t, srv, http.MethodPost, "/api/v1/conflicts/resolve", tok, api.ResolveConflictRequest{Subject: "s", Winning: "w"})
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for admin token, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
}

func TestConflictsResolveNotWiredReturns501(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{}) // no Actions attached
	tok := readToken(t, iss, "write")
	w := doJSON(t, srv, http.MethodPost, "/api/v1/conflicts/resolve", tok, api.ResolveConflictRequest{Subject: "s", Winning: "w"})
	if w.Result().StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501 when no action is wired, got %d", w.Result().StatusCode)
	}
}

// --- GET /api/v1/devices -----------------------------------------------------

func TestDevicesListRequiresToken(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{})
	w := doJSON(t, srv, http.MethodGet, "/api/v1/devices", "", nil)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Result().StatusCode)
	}
}

// TestDevicesListIncludesRegisteredAudioDevice proves a registered audio
// device (the kind daemon/system.Compose now mounts from
// daemon/audio.EnumerateEndpoints) actually shows up in the HTTP response —
// the route is a generic listing over whatever the namespace getter reports,
// not hardcoded to VRAM.
func TestDevicesListIncludesRegisteredAudioDevice(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{}).WithListGetters(api.ListGetters{
		Devices: func() []api.NamespaceDevice {
			return []api.NamespaceDevice{
				{Path: "/cer/dev/vram/local/0", Kind: "vram", Rights: []string{"read", "alloc"}},
				{Path: "/cer/dev/audio/mic/0", Kind: "audio", Rights: []string{"read"}},
				{Path: "/cer/dev/audio/speaker/0", Kind: "audio", Rights: []string{"read"}},
			}
		},
	})
	tok := readToken(t, iss, "read")
	w := doJSON(t, srv, http.MethodGet, "/api/v1/devices", tok, nil)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	var got []api.NamespaceDevice
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 devices, got %d: %+v", len(got), got)
	}
	foundAudio := false
	for _, d := range got {
		if d.Kind == "audio" && d.Path == "/cer/dev/audio/mic/0" {
			foundAudio = true
			if len(d.Rights) == 0 || d.Rights[0] != "read" {
				t.Fatalf("audio device rights wrong: %+v", d.Rights)
			}
		}
	}
	if !foundAudio {
		t.Fatalf("registered audio device did not appear in /api/v1/devices response: %+v", got)
	}
}

func TestDevicesListEmptyIsEmptyArray(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{})
	tok := readToken(t, iss, "read")
	w := doJSON(t, srv, http.MethodGet, "/api/v1/devices", tok, nil)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}
	var got []api.NamespaceDevice
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got == nil {
		t.Fatal("expected a non-nil (empty) slice so JSON is [] not null")
	}
}

// --- GET /api/v1/workloads ---------------------------------------------------

func TestWorkloadsListRequiresToken(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{})
	w := doJSON(t, srv, http.MethodGet, "/api/v1/workloads", "", nil)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Result().StatusCode)
	}
}

func TestWorkloadsListReturnsExpectedShape(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{}).WithListGetters(api.ListGetters{
		Workloads: func() []api.WorkloadEntry {
			return []api.WorkloadEntry{
				{ID: "abc123", Model: "hello-shard", Node: "local", State: "done"},
			}
		},
	})
	tok := readToken(t, iss, "read")
	w := doJSON(t, srv, http.MethodGet, "/api/v1/workloads", tok, nil)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(got))
	}
	for _, key := range []string{"id", "model", "node", "state"} {
		if _, ok := got[0][key]; !ok {
			t.Errorf("missing expected key %q in workload entry: %+v", key, got[0])
		}
	}
}

// --- POST /api/v1/cap/revoke -------------------------------------------------

func TestCapRevokeRequiresAdminToken(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{}).WithActions(api.Actions{
		RevokeCap: func(id string) (bool, error) { return true, nil },
	})
	// A write (but not admin) token must not satisfy the revoke route, mirroring
	// DaemonRPC.CapsRevoke's admin-only gate.
	writeTok := readToken(t, iss, "write")
	w := doJSON(t, srv, http.MethodPost, "/api/v1/cap/revoke", writeTok, api.RevokeRequest{ID: "tok1"})
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with write-only token, got %d", w.Result().StatusCode)
	}
	w = doJSON(t, srv, http.MethodPost, "/api/v1/cap/revoke", "", api.RevokeRequest{ID: "tok1"})
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d", w.Result().StatusCode)
	}
}

func TestCapRevokeWithAdminTokenSucceeds(t *testing.T) {
	iss, _ := auth.NewIssuer()
	var gotID string
	srv := api.NewWithGetters(iss, api.Getters{}).WithActions(api.Actions{
		RevokeCap: func(id string) (bool, error) {
			gotID = id
			return true, nil
		},
	})
	tok := readToken(t, iss, "admin")
	w := doJSON(t, srv, http.MethodPost, "/api/v1/cap/revoke", tok, api.RevokeRequest{ID: "tok-xyz"})
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	if gotID != "tok-xyz" {
		t.Fatalf("expected RevokeCap called with id=tok-xyz, got %q", gotID)
	}
	var resp api.RevokeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Revoked || resp.ID != "tok-xyz" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// --- POST /api/v1/devices/grant ---------------------------------------------

func TestDeviceGrantRequiresAdminToken(t *testing.T) {
	iss, _ := auth.NewIssuer()
	srv := api.NewWithGetters(iss, api.Getters{}).WithActions(api.Actions{
		GrantDevice: func(path string, rights []string) (string, string, string, error) {
			return "tok", "id1", "operator", nil
		},
	})
	writeTok := readToken(t, iss, "write")
	w := doJSON(t, srv, http.MethodPost, "/api/v1/devices/grant", writeTok, api.GrantDeviceRequest{Path: "/cer/dev/audio/mic/0"})
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with write-only token, got %d", w.Result().StatusCode)
	}
}

func TestDeviceGrantWithAdminTokenSucceeds(t *testing.T) {
	iss, _ := auth.NewIssuer()
	var gotPath string
	var gotRights []string
	srv := api.NewWithGetters(iss, api.Getters{}).WithActions(api.Actions{
		GrantDevice: func(path string, rights []string) (string, string, string, error) {
			gotPath, gotRights = path, rights
			return "minted-token", "cap-1", "operator", nil
		},
	})
	tok := readToken(t, iss, "admin")
	req := map[string]any{"path": "/cer/dev/audio/mic/0", "rights": []string{"read", "alloc"}}
	w := doJSON(t, srv, http.MethodPost, "/api/v1/devices/grant", tok, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	if gotPath != "/cer/dev/audio/mic/0" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if len(gotRights) != 2 || gotRights[0] != "read" || gotRights[1] != "alloc" {
		t.Fatalf("unexpected rights: %+v", gotRights)
	}
	var resp api.GrantDeviceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Token != "minted-token" || resp.ID != "cap-1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// GrantDeviceRequest.Rights may arrive as a bare string (the tray's UI is a
// single free-text field) rather than a JSON array; ParseRights must accept
// both encodings identically.
func TestDeviceGrantAcceptsStringRights(t *testing.T) {
	iss, _ := auth.NewIssuer()
	var gotRights []string
	srv := api.NewWithGetters(iss, api.Getters{}).WithActions(api.Actions{
		GrantDevice: func(path string, rights []string) (string, string, string, error) {
			gotRights = rights
			return "t", "i", "s", nil
		},
	})
	tok := readToken(t, iss, "admin")
	req := map[string]any{"path": "/cer/dev/audio/mic/0", "rights": "read, alloc"}
	w := doJSON(t, srv, http.MethodPost, "/api/v1/devices/grant", tok, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, w.Body.String())
	}
	if len(gotRights) != 2 || gotRights[0] != "read" || gotRights[1] != "alloc" {
		t.Fatalf("unexpected rights parsed from string form: %+v", gotRights)
	}
}
