package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
)

// ModelKind distinguishes WASM shard workloads from pipeline inference models.
type ModelKind string

const (
	// ModelKindWASM runs a WASM component through the executor (default).
	ModelKindWASM ModelKind = "wasm"
	// ModelKindInference runs a pipeline-backed inference model (split-MLP demo,
	// or future MLX / llama.cpp backends).
	ModelKindInference ModelKind = "inference"
)

// InferenceModelMeta is the minimal registry entry for an inference model.
type InferenceModelMeta struct {
	Backend    string `json:"backend,omitempty"`
	LayerCount uint32 `json:"layer_count,omitempty"`
	ModelPath  string `json:"model_path,omitempty"`
	Fixture    string `json:"fixture,omitempty"`
}

// InferenceMeta describes where/how an inference request ran.
type InferenceMeta struct {
	Backend string
	Node    string
}

// InferenceRunner executes pipeline-backed inference models. Implemented by
// daemon/system.InferenceService and wired at composition time.
type InferenceRunner interface {
	Run(ctx context.Context, subject, modelID string, input []byte) (content string, meta InferenceMeta, err error)
	RunStream(ctx context.Context, subject, modelID string, input []byte, emit func(token string) error) (meta InferenceMeta, err error)
}

// SetInference wires the optional pipeline inference runner. Nil disables the
// inference dispatch path (all models fall through to the WASM executor).
func (g *Gateway) SetInference(run InferenceRunner) {
	g.mu.Lock()
	g.inference = run
	g.mu.Unlock()
}

func (g *Gateway) inferenceRunner() InferenceRunner {
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

	content, meta, err := run.Run(r.Context(), subject, req.Model, nil)
	if err != nil {
		g.reportDispatchInference(subject, req.Model, meta.Node, false, err.Error())
		writeError(w, http.StatusBadGateway, "server_error", "inference failed: "+err.Error())
		return
	}
	g.reportDispatchInference(subject, req.Model, meta.Node, true, "")
	resp := ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   req.Model,
		Choices: []ChatChoice{{
			Index:        0,
			Message:      ChatMessage{Role: "assistant", Content: content},
			FinishReason: "stop",
		}},
		Usage: Usage{
			PromptTokens:     req.promptBytes(),
			CompletionTokens: len(content),
			TotalTokens:      req.promptBytes() + len(content),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (g *Gateway) streamInferenceChat(w http.ResponseWriter, r *http.Request, id string, req ChatRequest, subject string, run InferenceRunner, created int64) {
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

	meta, err := run.RunStream(r.Context(), subject, req.Model, nil, func(token string) error {
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		default:
		}
		if !writeChunk(chatChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []chatChunkChoice{{Index: 0, Delta: chatChunkDelta{Content: token}}},
		}) {
			return fmt.Errorf("stream write failed")
		}
		return nil
	})
	if err != nil {
		g.reportDispatchInference(subject, req.Model, meta.Node, false, err.Error())
		return
	}
	g.reportDispatchInference(subject, req.Model, meta.Node, true, "")

	stop := "stop"
	_ = writeChunk(chatChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []chatChunkChoice{{Index: 0, Delta: chatChunkDelta{}, FinishReason: &stop}},
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
