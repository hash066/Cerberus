package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// CompletionRequest is the subset of the legacy /v1/completions request we
// accept. Prompt may be a JSON string or an array of strings (OpenAI allows
// both); we accept either and join an array with newlines.
type CompletionRequest struct {
	Model  string      `json:"model"`
	Prompt promptField `json:"prompt"`
	Stream bool        `json:"stream"`
}

// CompletionChoice mirrors one entry of the legacy choices array.
type CompletionChoice struct {
	Index        int    `json:"index"`
	Text         string `json:"text"`
	FinishReason string `json:"finish_reason,omitempty"`
}

// CompletionResponse is the legacy text-completion response shape.
type CompletionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model,omitempty"`
	Choices []CompletionChoice `json:"choices"`
	Usage   Usage              `json:"usage"`
}

// promptField accepts either a JSON string or a JSON array of strings.
type promptField struct {
	text string
}

func (p *promptField) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		p.text = s
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	p.text = strings.Join(arr, "\n")
	return nil
}

func (req *CompletionRequest) validate() (int, string) {
	if strings.TrimSpace(req.Model) == "" {
		return http.StatusBadRequest, "missing required field: model"
	}
	if req.Prompt.text == "" {
		return http.StatusBadRequest, "missing required field: prompt"
	}
	if len(req.Prompt.text) > maxPromptBytes {
		return http.StatusRequestEntityTooLarge, "prompt too large"
	}
	return 0, ""
}

// HandleCompletions serves POST /v1/completions — the legacy text-completion
// endpoint. Same Bearer-cap gate, input hardening, executor dispatch, and
// optional SSE streaming as the chat endpoint; the result is returned as choice
// text rather than a chat message.
func (g *Gateway) HandleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	claims, ok := g.authorize(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req CompletionRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unexpected trailing data in body")
		return
	}
	if status, msg := req.validate(); status != 0 {
		writeError(w, status, "invalid_request_error", msg)
		return
	}

	result, err := g.dispatch(r.Context(), claims.Subject, req.Model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if !result.OK {
		msg := result.Error
		if msg == "" {
			msg = "workload produced no result"
		}
		writeError(w, http.StatusBadGateway, "server_error", "workload failed: "+msg)
		return
	}
	text := string(result.Output)
	created := time.Now().Unix()
	id := "cmpl-cerberus"

	if req.Stream {
		g.streamCompletion(w, r, id, req.Model, text, created)
		return
	}

	resp := CompletionResponse{
		ID:      id,
		Object:  "text_completion",
		Created: created,
		Model:   req.Model,
		Choices: []CompletionChoice{{Index: 0, Text: text, FinishReason: "stop"}},
		Usage: Usage{
			PromptTokens:     len(req.Prompt.text),
			CompletionTokens: len(text),
			TotalTokens:      len(req.Prompt.text) + len(text),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// completionChunk mirrors the legacy streaming text_completion chunk shape.
type completionChunk struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model,omitempty"`
	Choices []CompletionChoice `json:"choices"`
}

func (g *Gateway) streamCompletion(w http.ResponseWriter, r *http.Request, id, model, text string, created int64) {
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

	writeChunk := func(c completionChunk) bool {
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

	for _, piece := range chunkString(text, 64) {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		if !writeChunk(completionChunk{
			ID: id, Object: "text_completion", Created: created, Model: model,
			Choices: []CompletionChoice{{Index: 0, Text: piece}},
		}) {
			return
		}
	}
	_ = writeChunk(completionChunk{
		ID: id, Object: "text_completion", Created: created, Model: model,
		Choices: []CompletionChoice{{Index: 0, Text: "", FinishReason: "stop"}},
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}
