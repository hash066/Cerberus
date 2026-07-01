package gateway_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/gateway"
)

// shardExecutor mimics the real wazero executor: it returns the component's
// shard result (here the canonical 1337) as the output bytes, so we can assert
// the gateway surfaces a real compute result as content.
type shardExecutor struct{ value string }

func (e *shardExecutor) Dispatch(context.Context, contract.ComputeTask) (contract.PromiseHandle, error) {
	return contract.PromiseHandle(1), nil
}

func (e *shardExecutor) Resolve(context.Context, contract.PromiseHandle) (contract.ComputeResult, error) {
	v := e.value
	if v == "" {
		v = "1337"
	}
	return contract.ComputeResult{OK: true, Output: []byte(v)}, nil
}

func v3Token(t *testing.T) (*auth.Issuer, string) {
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

// --- /v1/chat/completions surfaces the real shard value (e.g. 1337) ----------

func TestChatSurfacesShardResult(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{value: "1337"}, iss)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"hello-shard","messages":[{"role":"user","content":"go"}]}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}
	var resp gateway.ChatResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "1337" {
		t.Fatalf("expected 1337 as content, got %+v", resp)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("expected finish_reason=stop, got %q", resp.Choices[0].FinishReason)
	}
	if resp.Object != "chat.completion" {
		t.Fatalf("expected object chat.completion, got %q", resp.Object)
	}
}

// --- streaming: SSE shape ----------------------------------------------------

func TestChatStreamingShape(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{value: "1337"}, iss)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"hello-shard","stream":true,"messages":[{"role":"user","content":"go"}]}`))
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
		sawRole    bool
		content    strings.Builder
		sawDone    bool
		sawFinish  bool
		dataFrames int
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
		dataFrames++
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
			t.Fatalf("bad chunk JSON %q: %v", payload, err)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("expected chat.completion.chunk, got %q", chunk.Object)
		}
		if len(chunk.Choices) == 0 {
			t.Fatalf("chunk has no choices: %q", payload)
		}
		d := chunk.Choices[0]
		if d.Delta.Role != "" {
			sawRole = true
		}
		content.WriteString(d.Delta.Content)
		if d.FinishReason != nil {
			sawFinish = true
			if *d.FinishReason != "stop" {
				t.Fatalf("expected finish_reason stop, got %q", *d.FinishReason)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if !sawRole {
		t.Fatal("expected an initial role delta")
	}
	if content.String() != "1337" {
		t.Fatalf("expected reassembled content 1337, got %q", content.String())
	}
	if !sawFinish {
		t.Fatal("expected a final finish_reason chunk")
	}
	if !sawDone {
		t.Fatal("expected a [DONE] sentinel")
	}
	if dataFrames < 3 {
		t.Fatalf("expected role+content+finish chunks, got %d data frames", dataFrames)
	}
}

func TestChatStreamingRequiresAuth(t *testing.T) {
	iss, _ := auth.NewIssuer()
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, req)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated stream, got %d", w.Result().StatusCode)
	}
}

// --- /v1/models --------------------------------------------------------------

func TestModelsRequiresAuth(t *testing.T) {
	iss, _ := auth.NewIssuer()
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w.Result().StatusCode)
	}
}

func TestModelsListsRegistered(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	gw.RegisterModel(gateway.Model{ID: "zeta-agent", ComponentCID: "bafy-zeta"})
	gw.RegisterModel(gateway.Model{ID: "hello-shard", ComponentCID: "bafy-hello"})
	gw.RegisterModel(gateway.Model{ID: ""}) // ignored

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID           string `json:"id"`
			Object       string `json:"object"`
			OwnedBy      string `json:"owned_by"`
			ComponentCID string `json:"component_cid"`
		} `json:"data"`
	}
	if err := json.NewDecoder(w.Result().Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" {
		t.Fatalf("expected object list, got %q", list.Object)
	}
	if len(list.Data) != 2 {
		t.Fatalf("expected 2 models (empty id ignored), got %d: %+v", len(list.Data), list.Data)
	}
	// Sorted by ID: hello-shard before zeta-agent.
	if list.Data[0].ID != "hello-shard" || list.Data[1].ID != "zeta-agent" {
		t.Fatalf("expected sorted ids, got %q,%q", list.Data[0].ID, list.Data[1].ID)
	}
	if list.Data[0].Object != "model" || list.Data[0].OwnedBy != "cerberus" {
		t.Fatalf("unexpected model object: %+v", list.Data[0])
	}
	if list.Data[0].ComponentCID != "bafy-hello" {
		t.Fatalf("expected component cid surfaced, got %q", list.Data[0].ComponentCID)
	}
}

func TestModelsRejectsWrongMethod(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST /v1/models, got %d", w.Result().StatusCode)
	}
}

// --- /v1/completions (legacy) ------------------------------------------------

func TestCompletionsSurfacesShardResult(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{value: "1337"}, iss)

	req := httptest.NewRequest(http.MethodPost, "/v1/completions",
		bytes.NewBufferString(`{"model":"hello-shard","prompt":"compute"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}
	var resp gateway.CompletionResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "text_completion" {
		t.Fatalf("expected text_completion, got %q", resp.Object)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Text != "1337" {
		t.Fatalf("expected 1337 text, got %+v", resp.Choices)
	}
}

func TestCompletionsAcceptsPromptArray(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{value: "42"}, iss)
	req := httptest.NewRequest(http.MethodPost, "/v1/completions",
		bytes.NewBufferString(`{"model":"m","prompt":["a","b"]}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for array prompt, got %d", w.Result().StatusCode)
	}
}

func TestCompletionsRejectsUnauthenticated(t *testing.T) {
	iss, _ := auth.NewIssuer()
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	req := httptest.NewRequest(http.MethodPost, "/v1/completions",
		bytes.NewBufferString(`{"model":"m","prompt":"x"}`))
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Result().StatusCode)
	}
}

func TestCompletionsInputValidation(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{}, iss)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing model", `{"prompt":"x"}`, http.StatusBadRequest},
		{"missing prompt", `{"model":"m"}`, http.StatusBadRequest},
		{"unknown field", `{"model":"m","prompt":"x","z":1}`, http.StatusBadRequest},
		{"trailing data", `{"model":"m","prompt":"x"}{}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewBufferString(tc.body))
			req.Header.Set("Authorization", "Bearer "+tok)
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, req)
			if w.Result().StatusCode != tc.want {
				t.Fatalf("body %q: expected %d, got %d", tc.body, tc.want, w.Result().StatusCode)
			}
		})
	}
}

// --- settlement seam ---------------------------------------------------------

type recordingSettler struct {
	calls   int32
	lastReq gateway.SettlementRequest
}

func (s *recordingSettler) Settle(_ context.Context, req gateway.SettlementRequest) (uint64, error) {
	atomic.AddInt32(&s.calls, 1)
	s.lastReq = req
	return 7, nil
}

func TestSettlerNoopByDefault(t *testing.T) {
	// With no settler and no pricing policy, a completed request must succeed and
	// record nothing (this is the default composition).
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{value: "1337"}, iss)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}
}

func TestSettlerInvokedForPricedTask(t *testing.T) {
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{value: "1337"}, iss)
	rec := &recordingSettler{}
	gw.SetSettler(rec)
	gw.SetPricingPolicy(func(task contract.ComputeTask, consumer string) (gateway.SettlementRequest, bool) {
		return gateway.SettlementRequest{
			Consumer: consumer,
			Provider: "node-b",
			Payment:  10,
			Bond:     5,
		}, true
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.HandleChatCompletions(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}
	if atomic.LoadInt32(&rec.calls) != 1 {
		t.Fatalf("expected settler called once, got %d", rec.calls)
	}
	if rec.lastReq.Provider != "node-b" || rec.lastReq.Consumer != "alice" {
		t.Fatalf("unexpected settlement req: %+v", rec.lastReq)
	}
	if rec.lastReq.OutputCID == "" {
		t.Fatal("expected output cid derived from result bytes")
	}
}

func TestSettlerSkippedWhenUnpriced(t *testing.T) {
	// A pricing policy that declines (priced=false) must not call the settler.
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{value: "1337"}, iss)
	rec := &recordingSettler{}
	gw.SetSettler(rec)
	gw.SetPricingPolicy(func(contract.ComputeTask, string) (gateway.SettlementRequest, bool) {
		return gateway.SettlementRequest{}, false
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/completions",
		bytes.NewBufferString(`{"model":"m","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Result().StatusCode)
	}
	if atomic.LoadInt32(&rec.calls) != 0 {
		t.Fatalf("expected settler NOT called for unpriced task, got %d", rec.calls)
	}
}

func TestSetSettlerNilRestoresNoop(t *testing.T) {
	// SetSettler(nil) must not panic and must leave the gateway functional.
	iss, tok := v3Token(t)
	gw := gateway.NewGateway(&shardExecutor{value: "1337"}, iss)
	gw.SetSettler(&recordingSettler{})
	gw.SetSettler(nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/completions",
		bytes.NewBufferString(`{"model":"m","prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after SetSettler(nil), got %d", w.Result().StatusCode)
	}
}
