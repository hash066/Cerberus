// Package gateway implements vertical 10's OpenAI-compatible HTTP front door.
//
// It is a drop-in `base_url` for existing agent SDKs, but with Cerberus's
// zero-ambient-authority posture: every request MUST present a capability token
// (Authorization: Bearer <token>) granting "exec". There is no unauthenticated
// path. Requests are mapped to a ComputeTask and dispatched through the executor
// (the scheduler/runtime place and run it across the mesh). The endpoint binds
// localhost only (ARCHITECTURE §1.1, vertical 10 §7) — no remote admin surface.
package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
)

const (
	// maxBodyBytes caps the request body so a hostile/buggy client can't exhaust
	// daemon memory. 1 MiB is generous for chat-completion JSON.
	maxBodyBytes = 1 << 20
	// maxMessages / maxContentBytes bound the work a single request can request.
	maxMessages     = 256
	maxContentBytes = 512 << 10 // 512 KiB total across all message contents
)

// Gateway is the OpenAI-compatible HTTP front door. Every request must present a
// capability token (Authorization: Bearer <token>) granting "exec" — there is no
// unauthenticated access, so multiple users/agents can share a daemon safely.
type Gateway struct {
	executor contract.Executor
	authz    auth.Authorizer
}

func NewGateway(executor contract.Executor, authz auth.Authorizer) *Gateway {
	return &Gateway{executor: executor, authz: authz}
}

// ChatMessage is one OpenAI-style chat message.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the subset of the OpenAI chat-completions request we accept.
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
}

// ChatChoice mirrors one entry of the OpenAI choices array.
type ChatChoice struct {
	Index   int         `json:"index"`
	Message ChatMessage `json:"message"`
}

// ChatResponse is the OpenAI-shaped chat-completion response.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model,omitempty"`
	Choices []ChatChoice `json:"choices"`
}

// errorResponse mirrors OpenAI's {"error": {...}} envelope so SDKs surface it.
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// writeError emits an OpenAI-shaped JSON error with the given status.
func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: errorBody{Message: msg, Type: typ}})
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

func (g *Gateway) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	// Capability auth: the caller must present a Bearer token granting "exec".
	// No token, wrong scheme, or insufficient rights → 401, before any parsing.
	tok := auth.BearerToken(r.Header.Get("Authorization"))
	if tok == "" {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "missing bearer token")
		return
	}
	claims, err := g.authz.Authorize(tok, "exec", "")
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "unauthorized: "+err.Error())
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

	// Map the request to a Cerberus ComputeTask scoped to the caller's subject.
	// The subject comes from the verified token, never from client-supplied input.
	task := contract.ComputeTask{
		TaskID:    []byte(claims.Subject + ":" + req.Model),
		Component: []byte("llm-component-cid"),
	}

	promise, err := g.executor.Dispatch(r.Context(), task)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	result, err := g.executor.Resolve(r.Context(), promise)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	resp := ChatResponse{
		ID:      "chatcmpl-cerberus",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []ChatChoice{{
			Index:   0,
			Message: ChatMessage{Role: "assistant", Content: string(result.Output)},
		}},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// Handler returns the gateway's HTTP mux (the OpenAI-compatible routes).
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", g.HandleChatCompletions)
	return mux
}

// Start serves the gateway on addr. addr SHOULD be a localhost address
// (e.g. "127.0.0.1:8080") — the gateway is not a remote admin surface
// (vertical 10 §7). Timeouts are set so a slow/stalled client can't pin a
// connection indefinitely.
func (g *Gateway) Start(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           g.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return srv.ListenAndServe()
}
