package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func newReq(token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
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

func TestGatewayAcceptsValidToken(t *testing.T) {
	iss, _ := auth.NewIssuer()
	gw := gateway.NewGateway(&mockExecutor{}, iss)
	tok, _ := iss.Mint("alice", []string{"exec"}, "", time.Hour)

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
