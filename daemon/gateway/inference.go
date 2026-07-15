package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
)

// ModelKind distinguishes WASM shard workloads from chat models.
type ModelKind string

const (
	// ModelKindWASM runs a WASM component through the executor (default).
	ModelKindWASM ModelKind = "wasm"
	// ModelKindInference runs a chat model through a ChatBackend.
	ModelKindInference ModelKind = "inference"
)

// InferenceModelMeta is the minimal registry entry for a chat model.
type InferenceModelMeta struct {
	Backend    string `json:"backend,omitempty"`
	LayerCount uint32 `json:"layer_count,omitempty"`
	ModelPath  string `json:"model_path,omitempty"`
	Fixture    string `json:"fixture,omitempty"`
}

// InferenceMeta describes where/how a request ran. Every field must be OBSERVED,
// never assumed — see ChatResult.
type InferenceMeta struct {
	Backend string
	Node    string
}

// ChatResult is what a backend actually did.
//
// Token counts are REAL counts from the engine, not byte lengths. The code this
// replaced reported `Usage.PromptTokens = req.promptBytes()` (a sum of message
// CHARACTER lengths) and `CompletionTokens = len(content)`. Those are not tokens
// and were wrong by a factor of ~4. llama-server reports genuine counts from its
// own tokenizer; anything that cannot must leave these zero rather than invent
// them.
type ChatResult struct {
	// Content is the assistant's reply.
	Content string
	// PromptTokens / CompletionTokens are real token counts, or 0 if the backend
	// genuinely cannot report them. Never a byte or character count.
	PromptTokens     int
	CompletionTokens int
	// Backend is the engine that ACTUALLY ran, as observed (e.g. "llama.cpp
	// b10021 vulkan"). Never a value computed from the local node's capabilities
	// and assumed to hold for remote work.
	Backend string
	// Nodes lists the nodes that actually served the request, main node first.
	Nodes []string
	// FinishReason is the engine's own stop reason ("stop", "length", ...).
	FinishReason string
}

func (r ChatResult) node() string {
	if len(r.Nodes) == 0 {
		return ""
	}
	return r.Nodes[0]
}

// ChatBackend runs a real chat completion.
//
// ############################ WHY THIS SHAPE ############################
//
// This REPLACES the old InferenceRunner:
//
//	Run(ctx, subject, modelID string, input []byte) (content string, ...)
//
// That signature was the ROOT CAUSE of the prompt-discard bug, not an incidental
// call-site slip. `input []byte` is structurally incapable of carrying a chat
// prompt — there is no field for messages, roles, or a conversation — so BOTH
// call sites in this file passed literal `nil` for it (the user's prompt was
// simply dropped on the floor), and the only way anything downstream could obtain
// text was to reach for the nearest string in scope. That is exactly what
// happened: daemon/system passed the authorization SUBJECT into a parameter named
// `prompt` and fed it to the model. Fixing the call sites alone would have left
// the next caller one refactor away from the same bug, so the interface changed.
//
// `subject` is still a parameter — it is the authorization principal, used for
// dispatch reporting and quota attribution. It MUST NEVER reach a model. A
// subject is an identity, not a prompt. The prompt lives in req.Messages and
// nowhere else.
//
// ########################################################################
type ChatBackend interface {
	// Chat runs a non-streaming completion.
	Chat(ctx context.Context, subject string, req ChatRequest) (ChatResult, error)
	// ChatStream runs a streaming completion, calling emit for each token delta as
	// the engine produces it. The returned ChatResult carries the final counts.
	ChatStream(ctx context.Context, subject string, req ChatRequest, emit func(delta string) error) (ChatResult, error)
}

// SetInference wires the optional chat backend. Nil disables the chat dispatch
// path (all models fall through to the WASM executor).
func (g *Gateway) SetInference(b ChatBackend) {
	g.mu.Lock()
	g.inference = b
	g.mu.Unlock()
}

func (g *Gateway) inferenceRunner() ChatBackend {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.inference
}

func (g *Gateway) isInferenceModel(model string) bool {
	g.mu.RLock()
	m, ok := g.models[model]
	g.mu.RUnlock()
	return ok && m.Kind == ModelKindInference
}

func (g *Gateway) handleInferenceChat(w http.ResponseWriter, r *http.Request, subject string, req ChatRequest) {
	run := g.inferenceRunner()
	if run == nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "inference runner not available")
		return
	}

	created := time.Now().Unix()
	id := "chatcmpl-cerberus"

	if req.Stream {
		g.streamInferenceChat(w, r, id, req, subject, run, created)
		return
	}

	// req carries the user's actual messages. This is the line that used to read
	// `run.Run(r.Context(), subject, req.Model, nil)` — dropping the prompt.
	res, err := run.Chat(r.Context(), subject, req)
	if err != nil {
		g.reportDispatchInference(subject, req.Model, res.node(), false, err.Error())
		writeError(w, http.StatusBadGateway, "server_error", "inference failed: "+err.Error())
		return
	}
	g.reportDispatchInference(subject, req.Model, res.node(), true, "")

	finish := res.FinishReason
	if finish == "" {
		finish = "stop"
	}
	resp := ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   req.Model,
		Choices: []ChatChoice{{
			Index:        0,
			Message:      ChatMessage{Role: "assistant", Content: res.Content},
			FinishReason: finish,
		}},
		// Real counts from the engine's own tokenizer.
		Usage: Usage{
			PromptTokens:     res.PromptTokens,
			CompletionTokens: res.CompletionTokens,
			TotalTokens:      res.PromptTokens + res.CompletionTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (g *Gateway) streamInferenceChat(w http.ResponseWriter, r *http.Request, id string, req ChatRequest, subject string, run ChatBackend, created int64) {
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming unsupported by transport")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	writeChunk := func(c chatChunk) bool {
		b, err := json.Marshal(c)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !writeChunk(chatChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []chatChunkChoice{{Index: 0, Delta: chatChunkDelta{Role: "assistant"}}},
	}) {
		return
	}

	// Each delta is a real token from the engine, relayed as it arrives — not a
	// synthetic per-stage progress line.
	res, err := run.ChatStream(r.Context(), subject, req, func(delta string) error {
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		default:
		}
		if !writeChunk(chatChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []chatChunkChoice{{Index: 0, Delta: chatChunkDelta{Content: delta}}},
		}) {
			return fmt.Errorf("stream write failed")
		}
		return nil
	})
	if err != nil {
		g.reportDispatchInference(subject, req.Model, res.node(), false, err.Error())
		return
	}
	g.reportDispatchInference(subject, req.Model, res.node(), true, "")

	finish := res.FinishReason
	if finish == "" {
		finish = "stop"
	}
	_ = writeChunk(chatChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []chatChunkChoice{{Index: 0, Delta: chatChunkDelta{}, FinishReason: &finish}},
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (g *Gateway) reportDispatchInference(subject, model, node string, ok bool, errMsg string) {
	task := contract.ComputeTask{TaskID: []byte(subject + ":inference:" + model)}
	if node == "" {
		node = "local"
	}
	g.reportDispatch(task, model, node, ok, errMsg)
}
