package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/gateway"
)

type mockExecutor struct{}

func (m *mockExecutor) Dispatch(ctx context.Context, t contract.ComputeTask) (contract.PromiseHandle, error) {
	return contract.PromiseHandle(1), nil
}

func (m *mockExecutor) Resolve(ctx context.Context, p contract.PromiseHandle) (contract.ComputeResult, error) {
	return contract.ComputeResult{Output: []byte("AI response")}, nil
}

const body = `{"model": "gpt", "messages": [{"role": "user", "content": "hi"}]}`

func newReqBody(token, b string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func newReq(token string) *http.Request { return newReqBody(token, body) }

// execToken mints a fresh issuer + an exec-capable token for the happy path.
func execToken(t *testing.T) (*auth.Issuer, string) {
	t.Helper()
	iss, err := auth.NewIssuer()
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	tok, err := iss.Mint("alice", []string{"exec"}, "", time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return iss, tok
}

func TestGatewayRejectsUnauthenticated(t *testing.T) {
	iss, _ := auth.NewIssuer()
	gw := gateway.NewGateway(&mockExecutor{}, iss)

	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, newReq(""))
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w.Result().StatusCode)
	}
}

func TestGatewayRejectsInsufficientRight(t *testing.T) {
	iss, _ := auth.NewIssuer()
	gw := gateway.NewGateway(&mockExecutor{}, iss)
	readOnly, _ := iss.Mint("alice", []string{"read"}, "", time.Hour)

	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, newReq(readOnly))
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for read-only token, got %d", w.Result().StatusCode)
	}
}

func TestGatewayRejectsBadSignature(t *testing.T) {
	// A token minted by a different issuer must not verify.
	iss, _ := auth.NewIssuer()
	gw := gateway.NewGateway(&mockExecutor{}, iss)
	other, _ := auth.NewIssuer()
	forged, _ := other.Mint("mallory", []string{"exec"}, "", time.Hour)

	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, newReq(forged))
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for foreign-signed token, got %d", w.Result().StatusCode)
	}
}

func TestGatewayAcceptsValidToken(t *testing.T) {
	iss, tok := execToken(t)
	gw := gateway.NewGateway(&mockExecutor{}, iss)

	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, newReq(tok))
	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with valid token, got %d", res.StatusCode)
	}
	var resp gateway.ChatResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "AI response" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestGatewayRejectsWrongMethod(t *testing.T) {
	iss, _ := auth.NewIssuer()
	gw := gateway.NewGateway(&mockExecutor{}, iss)
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, req)
	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", w.Result().StatusCode)
	}
}

func TestGatewayInputValidation(t *testing.T) {
	iss, tok := execToken(t)
	gw := gateway.NewGateway(&mockExecutor{}, iss)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing model", `{"messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest},
		{"empty messages", `{"model":"gpt","messages":[]}`, http.StatusBadRequest},
		{"missing content", `{"model":"gpt","messages":[{"role":"user"}]}`, http.StatusBadRequest},
		{"missing role", `{"model":"gpt","messages":[{"content":"hi"}]}`, http.StatusBadRequest},
		{"malformed json", `{"model":`, http.StatusBadRequest},
		{"unknown field", `{"model":"gpt","messages":[{"role":"user","content":"hi"}],"x":1}`, http.StatusBadRequest},
		{"trailing data", `{"model":"gpt","messages":[{"role":"user","content":"hi"}]}{}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			gw.HandleChatCompletions(w, newReqBody(tok, tc.body))
			if w.Result().StatusCode != tc.want {
				t.Fatalf("body %q: expected %d, got %d", tc.body, tc.want, w.Result().StatusCode)
			}
		})
	}
}

func TestGatewayRejectsOversizedBody(t *testing.T) {
	iss, tok := execToken(t)
	gw := gateway.NewGateway(&mockExecutor{}, iss)

	// > 1 MiB body should be rejected by MaxBytesReader before dispatch.
	huge := `{"model":"gpt","messages":[{"role":"user","content":"` +
		strings.Repeat("A", 2<<20) + `"}]}`
	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, newReqBody(tok, huge))
	if w.Result().StatusCode == http.StatusOK {
		t.Fatalf("expected oversized body to be rejected, got 200")
	}
}

func TestGatewayValidationRunsAfterAuth(t *testing.T) {
	// An invalid body with NO token must still 401 (auth before parsing), so we
	// never spend parsing effort on unauthenticated input.
	iss, _ := auth.NewIssuer()
	gw := gateway.NewGateway(&mockExecutor{}, iss)
	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, newReqBody("", `{garbage`))
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 (auth first), got %d", w.Result().StatusCode)
	}
}

// TestServeOverListener covers the Serve(net.Listener) variant cerberusd's
// main() now uses: the caller binds the listener itself first (so a bind
// conflict on the configured address is visible and can fall back to an
// ephemeral port), then hands the listener to Serve instead of letting
// Start's internal ListenAndServe swallow the bind step silently.
func TestServeOverListener(t *testing.T) {
	iss, tok := execToken(t)
	gw := gateway.NewGateway(&mockExecutor{}, iss)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- gw.Serve(ln) }()
	defer ln.Close()

	req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/models over bound listener: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/models over Serve(ln) should be 200, got %d", resp.StatusCode)
	}
}
