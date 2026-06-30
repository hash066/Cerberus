package gateway

import (
	"encoding/json"
	"net/http"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
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

type ChatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

type ChatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (g *Gateway) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Capability auth: the caller must present a Bearer token granting "exec".
	tok := auth.BearerToken(r.Header.Get("Authorization"))
	if tok == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	claims, err := g.authz.Authorize(tok, "exec", "")
	if err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Map the request to a Cerberus ComputeTask scoped to the caller's subject.
	task := contract.ComputeTask{
		TaskID:    []byte(claims.Subject + ":" + req.Model),
		Component: []byte("llm-component-cid"),
	}

	promise, err := g.executor.Dispatch(r.Context(), task)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result, err := g.executor.Resolve(r.Context(), promise)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := ChatResponse{ID: "chatcmpl-cerberus", Object: "chat.completion", Created: 1677652288}
	resp.Choices = append(resp.Choices, struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}{
		Index: 0,
		Message: struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: "assistant", Content: string(result.Output)},
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (g *Gateway) Start(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", g.HandleChatCompletions)
	return http.ListenAndServe(addr, mux)
}
