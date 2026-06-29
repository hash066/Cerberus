package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/gateway"
)

type mockExecutor struct{}

func (m *mockExecutor) Dispatch(ctx context.Context, t contract.ComputeTask) (contract.PromiseHandle, error) {
	return contract.PromiseHandle(1), nil
}

func (m *mockExecutor) Resolve(ctx context.Context, p contract.PromiseHandle) (contract.ComputeResult, error) {
	return contract.ComputeResult{Output: []byte("Mock AI response")}, nil
}

func TestGatewayCompletions(t *testing.T) {
	gw := gateway.NewGateway(&mockExecutor{})

	reqBody := `{"model": "gpt-3.5-turbo", "messages": [{"role": "user", "content": "hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	gw.HandleChatCompletions(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", res.StatusCode)
	}

	var resp gateway.ChatResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if len(resp.Choices) == 0 {
		t.Fatalf("Expected choices in response")
	}

	if resp.Choices[0].Message.Content != "Mock AI response" {
		t.Errorf("Expected 'Mock AI response', got '%s'", resp.Choices[0].Message.Content)
	}
}
