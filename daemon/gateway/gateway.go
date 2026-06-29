package gateway

import (
	"context"
	"encoding/json"
	"net/http"

	contract "github.com/hash066/cerberus/contract/go"
)

type Gateway struct {
	executor contract.Executor
}

func NewGateway(executor contract.Executor) *Gateway {
	return &Gateway{executor: executor}
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

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Map OpenAI request to a Cerberus ComputeTask (stub logic)
	task := contract.ComputeTask{
		TaskID:    []byte("task-123"),
		Component: []byte("llm-component-cid"),
	}

	// Dispatch the task via the executor (contract seam)
	promise, err := g.executor.Dispatch(context.Background(), task)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Wait for resolution
	result, err := g.executor.Resolve(context.Background(), promise)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Mocking response back to OpenAI format
	resp := ChatResponse{
		ID:      "chatcmpl-123",
		Object:  "chat.completion",
		Created: 1677652288,
	}
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
		}{
			Role: "assistant",
			// Pretend result contains the string
			Content: string(result.Output),
		},
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (g *Gateway) Start(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", g.HandleChatCompletions)
	return http.ListenAndServe(addr, mux)
}
