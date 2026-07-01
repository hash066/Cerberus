package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ChatMessage is one OpenAI-style chat message.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the subset of the OpenAI chat-completions request we accept.
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	// Stream, when true, switches the response to SSE (text/event-stream) with
	// chat.completion.chunk deltas terminated by a `data: [DONE]` line.
	Stream bool `json:"stream"`
}

// ChatChoice mirrors one entry of the OpenAI choices array (non-streaming).
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

// Usage mirrors OpenAI's token accounting. Cerberus doesn't tokenize like an LLM
// vendor; we report byte-based counts so the field is present and honest rather
// than fabricated token math.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse is the OpenAI-shaped chat-completion response.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model,omitempty"`
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
}

// chatChunkChoice / chatChunk mirror the streaming chat.completion.chunk shape.
type chatChunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type chatChunkChoice struct {
	Index        int            `json:"index"`
	Delta        chatChunkDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type chatChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model,omitempty"`
	Choices []chatChunkChoice `json:"choices"`
}

// validate enforces the request shape before we spend any compute on it.
func (req *ChatRequest) validate() (int, string) {
	if strings.TrimSpace(req.Model) == "" {
		return http.StatusBadRequest, "missing required field: model"
	}
	if len(req.Messages) == 0 {
		return http.StatusBadRequest, "messages must contain at least one message"
	}
	if len(req.Messages) > maxMessages {
		return http.StatusRequestEntityTooLarge, "too many messages"
	}
	total := 0
	for _, m := range req.Messages {
		if strings.TrimSpace(m.Role) == "" {
			return http.StatusBadRequest, "message is missing a role"
		}
		if m.Content == "" {
			return http.StatusBadRequest, "message is missing content"
		}
		total += len(m.Content)
		if total > maxContentBytes {
			return http.StatusRequestEntityTooLarge, "message content too large"
		}
	}
	return 0, ""
}

// promptBytes sums the request's message content length (for usage accounting).
func (req *ChatRequest) promptBytes() int {
	total := 0
	for _, m := range req.Messages {
		total += len(m.Content)
	}
	return total
}

// HandleChatCompletions serves POST /v1/chat/completions, non-streaming or (when
// {"stream":true}) as Server-Sent Events. Both paths run the requested component
// through the executor and surface its real result as the assistant message.
func (g *Gateway) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	// Capability auth first: no token / wrong scheme / insufficient rights → 401
	// before any parsing, so unauthenticated input costs us nothing.
	claims, ok := g.authorize(w, r)
	if !ok {
		return
	}

	// Bound the body and decode strictly: reject unknown fields and trailing data
	// so malformed/oversized input is refused rather than silently tolerated.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req ChatRequest
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

	// Dispatch the component. The subject comes from the verified token, never
	// from client-supplied input.
	result, err := g.dispatch(r.Context(), claims.Subject, req.Model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	content := string(result.Output)
	created := time.Now().Unix()
	id := "chatcmpl-cerberus"

	if req.Stream {
		g.streamChat(w, r, id, req, content, created)
		return
	}

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

// streamChat writes the completion as an OpenAI SSE stream: a role-priming chunk,
// one or more content-delta chunks, a final finish_reason chunk, then the
// terminating `data: [DONE]` line. Chunks are flushed as they are written so a
// client sees incremental output.
func (g *Gateway) streamChat(w http.ResponseWriter, r *http.Request, id string, req ChatRequest, content string, created int64) {
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		// No streaming transport (shouldn't happen over net/http); fall back to a
		// single non-streamed JSON body rather than lying about SSE support.
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

	// 1) Prime the assistant role.
	if !writeChunk(chatChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []chatChunkChoice{{Index: 0, Delta: chatChunkDelta{Role: "assistant"}}},
	}) {
		return
	}

	// 2) Emit the content. We chunk it so the surface is a genuine stream even
	// for a short result; the executor result is delivered whole (it is not a
	// token-by-token model), so this is transport chunking, not fake tokens.
	for _, piece := range chunkString(content, 64) {
		select {
		case <-r.Context().Done():
			return // client hung up
		default:
		}
		if !writeChunk(chatChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []chatChunkChoice{{Index: 0, Delta: chatChunkDelta{Content: piece}}},
		}) {
			return
		}
	}

	// 3) Final chunk with finish_reason, then the DONE sentinel.
	stop := "stop"
	_ = writeChunk(chatChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []chatChunkChoice{{Index: 0, Delta: chatChunkDelta{}, FinishReason: &stop}},
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// chunkString splits s into pieces of at most n bytes (rune-safe boundaries are
// not required here: the content is emitted verbatim and reassembled by the
// client, and SSE data is byte-transparent). An empty string yields no chunks.
func chunkString(s string, n int) []string {
	if s == "" {
		return nil
	}
	if n <= 0 {
		return []string{s}
	}
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	out = append(out, s)
	return out
}
