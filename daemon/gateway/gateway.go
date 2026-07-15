// Package gateway implements vertical 10's OpenAI-compatible HTTP front door.
//
// It is a drop-in `base_url` for existing agent SDKs, but with Cerberus's
// zero-ambient-authority posture: every request MUST present a capability token
// (Authorization: Bearer <token>) granting "exec". There is no unauthenticated
// path. Requests are mapped to a ComputeTask and dispatched through the executor
// (the scheduler/runtime place and run it across the mesh). The endpoint binds
// localhost only (ARCHITECTURE §1.1, vertical 10 §7) — no remote admin surface.
//
// v3 surface:
//   - POST /v1/chat/completions   non-streaming (chat.completion) AND streaming
//     via SSE (text/event-stream, chat.completion.chunk) when {"stream":true}.
//   - GET  /v1/models             lists the available components/agents as models.
//   - POST /v1/completions        the legacy text-completion shape.
//
// Every route runs the requested component through the executor and surfaces the
// real shard result (e.g. the 1337 value) as the assistant/text content — this
// is a real dispatch, not a canned string.
package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
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
	// maxPromptBytes bounds a legacy /v1/completions prompt.
	maxPromptBytes = 512 << 10
)

// Model is one entry the gateway advertises on /v1/models. A Cerberus "model" is
// either a WASM component/agent addressable by CID, or a pipeline inference model.
// ID is the string an OpenAI client passes as "model".
type Model struct {
	// ID is the OpenAI-facing model name (what a client sets as "model").
	ID string
	// Kind selects the dispatch path: WASM executor (default) or pipeline inference.
	Kind ModelKind
	// ComponentCID is the content id of the WASM component this model runs.
	// Surfaced to clients (as metadata) and used to build the ComputeTask.
	ComponentCID string
	// Inference holds registry metadata when Kind is ModelKindInference.
	Inference InferenceModelMeta
	// OwnedBy is the "owned_by" field OpenAI clients expect (default "cerberus").
	OwnedBy string
	// Created is the advertised creation time (unix seconds); 0 → gateway start.
	Created int64
}

// DispatchEvent describes one completed (or failed) gateway dispatch, for the
// optional OnDispatch hook below. It carries just enough for a workload
// history view (the tray's /api/v1/workloads) without the gateway needing to
// know anything about how that history is stored.
type DispatchEvent struct {
	TaskID string // hex/opaque task identifier, as recorded in the ComputeTask
	Model  string // the OpenAI-facing model name the client requested
	Node   string // "local" or hex peer id where the task ran
	OK     bool
	Error  string
}

// OnDispatchFunc is called once per gateway dispatch attempt (success or
// failure). It must not block meaningfully — the gateway calls it inline,
// synchronously, after resolving (or failing to resolve) the task, before
// writing the HTTP response.
type OnDispatchFunc func(DispatchEvent)

// Gateway is the OpenAI-compatible HTTP front door. Every request must present a
// capability token (Authorization: Bearer <token>) granting "exec" — there is no
// unauthenticated access, so multiple users/agents can share a daemon safely.
type Gateway struct {
	executor  contract.Executor
	authz     auth.Authorizer
	inference ChatBackend

	mu         sync.RWMutex
	models     map[string]Model // keyed by Model.ID
	settler    Settler          // optional; no-op when unset
	pricing    PricingPolicy    // optional; nil = nothing is priced
	onDispatch OnDispatchFunc   // optional; nil = no workload-history hook
	started    int64
}

// NewGateway builds a gateway over the given executor and authorizer. It starts
// with no advertised models and a no-op settler; the composition (LEAD) wires
// real models via RegisterModel and a real settler via SetSettler.
func NewGateway(executor contract.Executor, authz auth.Authorizer) *Gateway {
	return &Gateway{
		executor: executor,
		authz:    authz,
		models:   map[string]Model{},
		settler:  noopSettler{},
		started:  time.Now().Unix(),
	}
}

// RegisterModel advertises a component/agent as an OpenAI model. Re-registering
// the same ID replaces it. Safe for concurrent use. A model with an empty ID is
// ignored. This is how the composition exposes the mesh's components on
// /v1/models; a request naming an unregistered model still dispatches (the
// executor decides), so the registry is advisory, not a gate.
func (g *Gateway) RegisterModel(m Model) {
	if strings.TrimSpace(m.ID) == "" {
		return
	}
	if m.OwnedBy == "" {
		m.OwnedBy = "cerberus"
	}
	if m.Created == 0 {
		m.Created = g.started
	}
	g.mu.Lock()
	g.models[m.ID] = m
	g.mu.Unlock()
}

// SetOnDispatch wires an optional workload-history hook, called once per
// dispatch attempt (success or failure) after the executor has resolved (or
// failed to resolve) the task. Passing nil disables the hook. This is the
// seam the composition (cmd/cerberusd/main.go) uses to append to the
// workload-history log the new /api/v1/workloads route reads — the gateway
// itself has no notion of "history", it just reports what happened.
func (g *Gateway) SetOnDispatch(fn OnDispatchFunc) {
	g.mu.Lock()
	g.onDispatch = fn
	g.mu.Unlock()
}

// listModels returns the advertised models in a stable (ID-sorted) order.
func (g *Gateway) listModels() []Model {
	g.mu.RLock()
	out := make([]Model, 0, len(g.models))
	for _, m := range g.models {
		out = append(out, m)
	}
	g.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// componentFor resolves the WASM component bytes/CID for a requested model name.
// If the model is registered its ComponentCID is used; otherwise the raw model
// string is passed through so the executor can still attempt a dispatch (keeping
// the gateway usable before any model is registered).
func (g *Gateway) componentFor(model string) []byte {
	g.mu.RLock()
	m, ok := g.models[model]
	g.mu.RUnlock()
	if ok && m.ComponentCID != "" {
		return []byte(m.ComponentCID)
	}
	return []byte(model)
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

// authorize runs the shared Bearer-cap gate for every route: the caller must
// present a token granting "exec". Returns the verified claims, or writes a 401
// and returns ok=false. Auth runs before any body parsing so we never spend
// effort on unauthenticated input.
func (g *Gateway) authorize(w http.ResponseWriter, r *http.Request) (auth.Claims, bool) {
	tok := auth.BearerToken(r.Header.Get("Authorization"))
	if tok == "" {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "missing bearer token")
		return auth.Claims{}, false
	}
	claims, err := g.authz.Authorize(tok, "exec", "")
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "unauthorized: "+err.Error())
		return auth.Claims{}, false
	}
	return claims, true
}

// dispatch runs the requested component through the executor and returns the
// resolved result. The subject is taken from the verified token, never from
// client input, so a request can only ever run as its authenticated principal.
func (g *Gateway) dispatch(ctx context.Context, subject, model string) (contract.ComputeResult, error) {
	task := contract.ComputeTask{
		TaskID:    []byte(subject + ":" + model),
		Component: g.componentFor(model),
	}
	promise, err := g.executor.Dispatch(ctx, task)
	if err != nil {
		g.reportDispatch(task, model, "local", false, err.Error())
		return contract.ComputeResult{}, err
	}
	res, err := g.executor.Resolve(ctx, promise)
	node := dispatchNode(g.executor, promise)
	if err != nil {
		g.reportDispatch(task, model, node, false, err.Error())
		return contract.ComputeResult{}, err
	}
	// Record settlement for a completed, priced task (no-op unless a real
	// settler was wired). Never fails the request: settlement is a side effect
	// of a successful compute, not part of the response contract.
	g.recordSettlement(ctx, task, subject, res)
	g.reportDispatch(task, model, node, res.OK, res.Error)
	return res, nil
}

// dispatchNode returns where a promise ran when the executor exposes LastWhere.
func dispatchNode(exec contract.Executor, p contract.PromiseHandle) string {
	type whereProvider interface {
		LastWhere(contract.PromiseHandle) string
	}
	if wp, ok := exec.(whereProvider); ok {
		if w := wp.LastWhere(p); w != "" {
			return w
		}
	}
	return "local"
}

// reportDispatch invokes the optional OnDispatch hook (if one is wired), never
// blocking the request on it. A panic-free, error-free side channel: the hook
// itself may do anything (append to a ring buffer, etc.) but a slow/faulty
// hook is the composition's problem to fix, not something dispatch guards
// against here (mirrors recordSettlement's "never fails the request" posture).
func (g *Gateway) reportDispatch(task contract.ComputeTask, model, node string, ok bool, errMsg string) {
	g.mu.RLock()
	hook := g.onDispatch
	g.mu.RUnlock()
	if hook == nil {
		return
	}
	if node == "" {
		node = "local"
	}
	hook(DispatchEvent{TaskID: string(task.TaskID), Model: model, Node: node, OK: ok, Error: errMsg})
}

// Handler returns the gateway's HTTP mux (the OpenAI-compatible routes).
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", g.HandleChatCompletions)
	mux.HandleFunc("/v1/completions", g.HandleCompletions)
	mux.HandleFunc("/v1/models", g.HandleModels)
	return mux
}

// Start serves the gateway on addr. addr SHOULD be a localhost address
// (e.g. "127.0.0.1:8080") — the gateway is not a remote admin surface
// (vertical 10 §7). Timeouts are set so a slow/stalled client can't pin a
// connection indefinitely.
//
// WriteTimeout bounds a whole response including a streamed SSE body, so it is
// set generously; a client that holds a stream open past it is cut off by
// design (a localhost gateway is not a long-poll server).
func (g *Gateway) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return g.Serve(ln)
}

// Serve is like Start but serves on an already-bound net.Listener instead of
// calling http.Server.ListenAndServe internally. This lets a caller bind the
// listener itself first (net.Listen), inspect/react to a bind failure (e.g.
// fall back to an ephemeral port on a conflict), and log the real bound
// address — none of which is observable through the opaque ListenAndServe
// path. Start is kept as a thin wrapper over this for backward compatibility.
func (g *Gateway) Serve(ln net.Listener) error {
	srv := &http.Server{
		Handler:           g.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}
	return srv.Serve(ln)
}
