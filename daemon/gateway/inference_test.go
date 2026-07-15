package gateway_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hash066/cerberus/daemon/gateway"
)

type mockInference struct {
	content string
	tokens  []string
	node    string
}

func (m *mockInference) Run(_ context.Context, _, _ string, _ []byte) (string, gateway.InferenceMeta, error) {
	return m.content, gateway.InferenceMeta{Backend: "cpu-software", Node: m.node}, nil
}

func (m *mockInference) RunStream(_ context.Context, _, _ string, _ []byte, emit func(token string) error) (gateway.InferenceMeta, error) {
	for _, tok := range m.tokens {
		if err := emit(tok); err != nil {
			return gateway.InferenceMeta{Backend: "cpu-software", Node: m.node}, err
		}
	}
	return gateway.InferenceMeta{Backend: "cpu-software", Node: m.node}, nil
}

func TestInferenceChatNonStreaming(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	gw.SetInference(&mockInference{
		content: "[split-mlp cpu-software] activation: [0.1000, 0.0000, 0.0000, 0.0000]",
		node:    "local",
	})
	gw.RegisterModel(gateway.Model{
		ID:   "split-mlp-demo",
		Kind: gateway.ModelKindInference,
		Inference: gateway.InferenceModelMeta{
			Backend: "cpu-software",
			Fixture: "splitmlp",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"split-mlp-demo","messages":[{"role":"user","content":"run"}]}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Result().StatusCode, w.Body.String())
	}
	var resp gateway.ChatResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Choices[0].Message.Content, "split-mlp") {
		t.Fatalf("expected inference content, got %q", resp.Choices[0].Message.Content)
	}
}

func TestInferenceChatStreamingShape(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	gw.SetInference(&mockInference{
		tokens: []string{"[stage L0-1 @ deadbeef ok]\n", "[split-mlp cpu-software] activation: [1,2,3,4]"},
		node:   "deadbeef",
	})
	gw.RegisterModel(gateway.Model{ID: "split-mlp-demo", Kind: gateway.ModelKindInference})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"split-mlp-demo","stream":true,"messages":[{"role":"user","content":"run"}]}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	var (
		sawRole   bool
		content   strings.Builder
		sawDone   bool
		sawFinish bool
	)
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad chunk: %v", err)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("unexpected object %q", chunk.Object)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if chunk.Choices[0].Delta.Role != "" {
			sawRole = true
		}
		content.WriteString(chunk.Choices[0].Delta.Content)
		if chunk.Choices[0].FinishReason != nil {
			sawFinish = true
		}
	}
	if !sawRole || !sawFinish || !sawDone {
		t.Fatalf("stream shape incomplete: role=%v finish=%v done=%v", sawRole, sawFinish, sawDone)
	}
	if !strings.Contains(content.String(), "stage L0-1") {
		t.Fatalf("expected stage token in stream, got %q", content.String())
	}
}

func TestWasmPathUnchangedForRegisteredWasmModel(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{value: "1337"}, iss)
	gw.SetInference(&mockInference{content: "should-not-run"})
	gw.RegisterModel(gateway.Model{ID: "hello-shard", ComponentCID: "hello-shard", Kind: gateway.ModelKindWASM})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"hello-shard","messages":[{"role":"user","content":"go"}]}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, req)

	var resp gateway.ChatResponse
	_ = json.NewDecoder(w.Result().Body).Decode(&resp)
	if resp.Choices[0].Message.Content != "1337" {
		t.Fatalf("expected wasm path 1337, got %q", resp.Choices[0].Message.Content)
	}
}

func TestInferenceModelsListed(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	gw.RegisterModel(gateway.Model{
		ID:   "split-mlp-demo",
		Kind: gateway.ModelKindInference,
		Inference: gateway.InferenceModelMeta{
			Backend:    "cpu-software",
			LayerCount: 4,
			Fixture:    "splitmlp",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)

	var list struct {
		Data []struct {
			ID        string                      `json:"id"`
			Kind      string                      `json:"kind"`
			Inference *gateway.InferenceModelMeta `json:"inference"`
		} `json:"data"`
	}
	if err := json.NewDecoder(w.Result().Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0].Kind != "inference" || list.Data[0].Inference == nil {
		t.Fatalf("expected inference model metadata, got %+v", list.Data)
	}
}

func TestInferenceRequiresRunner(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	gw.RegisterModel(gateway.Model{ID: "split-mlp-demo", Kind: gateway.ModelKindInference})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"split-mlp-demo","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, req)
	if w.Result().StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without runner, got %d", w.Result().StatusCode)
	}
}
