package gateway_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hash066/cerberus/daemon/gateway"
)

// mockInference records what the gateway ACTUALLY handed it. The recording is the
// point: the bug this interface replaced was the gateway passing `nil` where the
// prompt should have been, and no test noticed because no test ever looked at what
// the backend received.
type mockInference struct {
	mu       sync.Mutex
	content  string
	tokens   []string
	node     string
	promptTk int
	compTk   int

	gotReq     gateway.ChatRequest
	gotSubject string
	called     bool
}

func (m *mockInference) record(subject string, req gateway.ChatRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gotReq = req
	m.gotSubject = subject
	m.called = true
}

func (m *mockInference) seen() (gateway.ChatRequest, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gotReq, m.gotSubject, m.called
}

func (m *mockInference) result() gateway.ChatResult {
	return gateway.ChatResult{
		Content:          m.content,
		PromptTokens:     m.promptTk,
		CompletionTokens: m.compTk,
		Backend:          "cpu-software",
		Nodes:            []string{m.node},
		FinishReason:     "stop",
	}
}

func (m *mockInference) Chat(_ context.Context, subject string, req gateway.ChatRequest) (gateway.ChatResult, error) {
	m.record(subject, req)
	return m.result(), nil
}

func (m *mockInference) ChatStream(_ context.Context, subject string, req gateway.ChatRequest, emit func(string) error) (gateway.ChatResult, error) {
	m.record(subject, req)
	for _, tok := range m.tokens {
		if err := emit(tok); err != nil {
			return m.result(), err
		}
	}
	return m.result(), nil
}

// TestInferenceChatDeliversThePromptToTheBackend is the regression test for the
// prompt-discard bug, and it is the one that should have existed all along.
//
// The old gateway called `run.Run(ctx, subject, req.Model, nil)` — the user's
// messages were dropped on the floor, and because the old interface's payload was
// `input []byte` there was nowhere to put them even if someone had tried. The
// downstream code then reached for the nearest string in scope, which was the
// authorization SUBJECT, and fed that to the model.
//
// So this asserts BOTH halves:
//  1. the user's actual prompt arrives at the backend, and
//  2. the subject does NOT appear anywhere in the messages.
func TestInferenceChatDeliversThePromptToTheBackend(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	mock := &mockInference{content: "hello back", node: "local", promptTk: 7, compTk: 3}
	gw.SetInference(mock)
	gw.RegisterModel(gateway.Model{ID: "m", Kind: gateway.ModelKindInference})

	const prompt = "what is the capital of France?"
	body := `{"model":"m","messages":[{"role":"user","content":"` + prompt + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Result().StatusCode, w.Body.String())
	}

	got, subject, called := mock.seen()
	if !called {
		t.Fatal("the backend was never called")
	}
	if len(got.Messages) == 0 {
		t.Fatal("the backend received NO messages — the prompt was discarded (this is the exact " +
			"bug the ChatBackend interface exists to make impossible)")
	}
	if got.Messages[0].Content != prompt {
		t.Fatalf("backend received %q, want the user's actual prompt %q", got.Messages[0].Content, prompt)
	}
	if got.Messages[0].Role != "user" {
		t.Fatalf("role = %q, want user", got.Messages[0].Role)
	}
	// The subject is an authorization principal. It must never be presented to a
	// model as text.
	for _, m := range got.Messages {
		if subject != "" && strings.Contains(m.Content, subject) {
			t.Fatalf("the auth subject %q leaked into a model message (%q) — a subject is an "+
				"identity, not a prompt", subject, m.Content)
		}
	}
}

// TestInferenceChatReportsRealTokenCounts pins that usage comes from the backend.
// The old code reported Usage.PromptTokens = sum of message CHARACTER lengths and
// CompletionTokens = len(content) — byte counts wearing the name "tokens", wrong
// by roughly 4x.
func TestInferenceChatReportsRealTokenCounts(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	// A 30-character prompt that is really 7 tokens, and a 10-character reply that
	// is really 3. Byte-counting would report 30/10.
	gw.SetInference(&mockInference{content: "0123456789", node: "local", promptTk: 7, compTk: 3})
	gw.RegisterModel(gateway.Model{ID: "m", Kind: gateway.ModelKindInference})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"m","messages":[{"role":"user","content":"123456789012345678901234567890"}]}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, req)

	var resp gateway.ChatResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Usage.PromptTokens != 7 {
		t.Fatalf("PromptTokens = %d, want 7 (the engine's real count, not a character count)", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 3 {
		t.Fatalf("CompletionTokens = %d, want 3 (not len(content)=10)", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 10 {
		t.Fatalf("TotalTokens = %d, want 10", resp.Usage.TotalTokens)
	}
}

func TestInferenceChatNonStreaming(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	gw.SetInference(&mockInference{content: "a real reply", node: "local"})
	gw.RegisterModel(gateway.Model{
		ID:        "m",
		Kind:      gateway.ModelKindInference,
		Inference: gateway.InferenceModelMeta{Backend: "cpu-software"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"m","messages":[{"role":"user","content":"run"}]}`))
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
	if resp.Choices[0].Message.Content != "a real reply" {
		t.Fatalf("got %q", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
}

// TestInferenceChatStreamingRelaysTokenDeltas: each SSE delta is a real token from
// the engine, relayed as it arrives.
func TestInferenceChatStreamingShape(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	gw.SetInference(&mockInference{
		tokens: []string{"The ", "capital ", "is ", "Paris."},
		node:   "deadbeef",
	})
	gw.RegisterModel(gateway.Model{ID: "m", Kind: gateway.ModelKindInference})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"m","stream":true,"messages":[{"role":"user","content":"run"}]}`))
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
	// Real generated text, reassembled from token deltas — not per-stage progress
	// lines, which is what the fixture path used to emit here.
	if content.String() != "The capital is Paris." {
		t.Fatalf("reassembled stream = %q, want the model's tokens", content.String())
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
		ID:   "some-gguf",
		Kind: gateway.ModelKindInference,
		Inference: gateway.InferenceModelMeta{
			Backend: "llama.cpp b10021",
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
	gw.RegisterModel(gateway.Model{ID: "m", Kind: gateway.ModelKindInference})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, req)
	if w.Result().StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without runner, got %d", w.Result().StatusCode)
	}
}
