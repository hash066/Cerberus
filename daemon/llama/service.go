package llama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hash066/cerberus/daemon/gateway"
)

// service.go is the REQUESTER half: it runs llama-server locally (optionally with
// remote workers behind the forwarder) and proxies its OpenAI-compatible endpoint.
//
// WHY PROXY llama-server INSTEAD OF DRIVING libllama:
//
// llama-server already implements, correctly and under test, every piece that was
// missing from this repo: a real tokenizer, GGUF parsing, weight loading, a
// KV-cache, a sampling loop, streaming, stop conditions, and genuine `usage` token
// counts. Reimplementing any of that in Go would be reimplementing llama.cpp
// badly, and the last attempt to fake it is what this lane deleted. So Cerberus
// contributes the capability-gated transport and supervision, and llama.cpp
// contributes the inference. There is no tokenizer in this package on purpose.

// ServiceConfig configures the local llama-server.
type ServiceConfig struct {
	// ModelPath is the GGUF to load. ONLY the main node needs this file: llama.cpp
	// pushes tensors to remote workers at load time, so a worker needs the binary
	// but not the weights. That is a genuine UX win and worth stating.
	ModelPath string
	// ModelID is the id advertised on /v1/models.
	ModelID string
	// RPCServers is the --rpc value (comma-separated host:port), normally the
	// forwarder's loopback endpoints. Empty runs single-node.
	RPCServers string
	// GPULayers maps to -ngl. Default 99 (offload everything possible).
	GPULayers int
	// Port for llama-server. Zero reserves one on loopback.
	Port int
	// Logf receives lifecycle lines. Nil uses the standard logger.
	Logf func(format string, args ...any)
	// OnExit is called when the llama-server child exits. The daemon uses it to
	// tear the forwarder down, so its unauthenticated loopback ports do not
	// outlive the run that needed them (see doc.go).
	OnExit func()
}

// Service supervises one llama-server and proxies chat completions to it.
type Service struct {
	cfg   ServiceConfig
	bins  Binaries
	logf  func(string, ...any)
	Nodes []string // observed nodes serving this model, main node first

	mu      sync.Mutex
	cmd     *exec.Cmd
	baseURL string
	started bool
}

// NewService locates version-gated binaries and prepares a llama-server
// supervisor. It does NOT start the child — call Start.
//
// It returns ErrPackMissing when no pack is installed, so a caller can offer to
// fetch one rather than treat it as fatal.
func NewService(cfg ServiceConfig) (*Service, error) {
	if strings.TrimSpace(cfg.ModelPath) == "" {
		return nil, fmt.Errorf("llama: NewService requires a GGUF model path (only the main node needs it)")
	}
	if _, err := os.Stat(cfg.ModelPath); err != nil {
		return nil, fmt.Errorf("llama: model %q is not readable: %w", cfg.ModelPath, err)
	}
	bins, err := Locate(context.Background())
	if err != nil {
		return nil, err
	}
	if cfg.GPULayers == 0 {
		cfg.GPULayers = 99
	}
	if cfg.ModelID == "" {
		cfg.ModelID = "local-gguf"
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Service{cfg: cfg, bins: bins, logf: logf}, nil
}

// serverArgs builds the llama-server argv.
//
// Verified against upstream common/arg.cpp: `--rpc SERVERS` is a
// "comma-separated list of RPC servers (host:port)". --host is pinned to loopback
// for the same reason ggml-rpc-server is: the gateway is Cerberus's authenticated
// front door, and llama-server behind it must not be independently reachable.
func (s *Service) serverArgs(port int) []string {
	args := []string{
		"-m", s.cfg.ModelPath,
		"--host", loopbackHost,
		"--port", strconv.Itoa(port),
		"-ngl", strconv.Itoa(s.cfg.GPULayers),
	}
	if s.cfg.RPCServers != "" {
		args = append(args, "--rpc", s.cfg.RPCServers)
	}
	return args
}

// Start launches llama-server and waits for its /health to report ready.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}

	port := s.cfg.Port
	if port == 0 {
		p, err := reservePort()
		if err != nil {
			return err
		}
		port = p
	}

	args := s.serverArgs(port)
	cmd := exec.Command(s.bins.Server, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("llama: start llama-server: %w", err)
	}
	s.cmd = cmd
	s.baseURL = "http://" + loopbackHost + ":" + strconv.Itoa(port)

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
		if s.cfg.OnExit != nil {
			s.cfg.OnExit()
		}
	}()

	if err := s.waitHealthy(ctx, done); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	s.started = true

	where := "single node"
	if s.cfg.RPCServers != "" {
		where = "distributed over " + s.cfg.RPCServers
	}
	s.logf("llama: llama-server ready on %s (llama.cpp b%d, %s)", s.baseURL, s.bins.Build, where)
	return nil
}

// waitHealthy polls llama-server's /health. Loading a large GGUF and pushing
// tensors to remote workers is slow, so the budget is generous; the child dying is
// detected immediately rather than waited out.
func (s *Service) waitHealthy(ctx context.Context, done <-chan struct{}) error {
	deadline := time.Now().Add(10 * time.Minute)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-done:
			return fmt.Errorf("llama: llama-server exited before becoming healthy (see its log above)")
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/health", nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("llama: llama-server did not become healthy within 10m")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Stop terminates llama-server.
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	s.started = false
}

// BackendName is the ACTUAL backend string, built from what was observed: the
// located binaries' real build number, and whether remote workers are genuinely in
// use. It never guesses.
func (s *Service) BackendName() string {
	b := "llama.cpp b" + strconv.Itoa(s.bins.Build)
	if s.cfg.RPCServers != "" {
		b += " +rpc"
	}
	return b
}

// ---- the OpenAI proxy ----

type upstreamMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type upstreamRequest struct {
	Model    string            `json:"model"`
	Messages []upstreamMessage `json:"messages"`
	Stream   bool              `json:"stream"`
}

type upstreamUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type upstreamChoice struct {
	Index        int             `json:"index"`
	Message      upstreamMessage `json:"message"`
	Delta        upstreamMessage `json:"delta"`
	FinishReason string          `json:"finish_reason"`
}

type upstreamResponse struct {
	Choices []upstreamChoice `json:"choices"`
	Usage   upstreamUsage    `json:"usage"`
}

// toUpstream converts a gateway ChatRequest into llama-server's request.
//
// NOTE the absence of `subject`: the authorization principal is deliberately NOT
// forwarded. The bug this lane fixed was a subject reaching the model as its
// prompt. Only req.Messages carries text.
func toUpstream(modelID string, req gateway.ChatRequest, stream bool) upstreamRequest {
	msgs := make([]upstreamMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, upstreamMessage{Role: m.Role, Content: m.Content})
	}
	return upstreamRequest{Model: modelID, Messages: msgs, Stream: stream}
}

// Chat proxies a non-streaming completion. subject is accepted for the interface's
// sake and used for nothing that reaches the model.
func (s *Service) Chat(ctx context.Context, _ string, req gateway.ChatRequest) (gateway.ChatResult, error) {
	if len(req.Messages) == 0 {
		return gateway.ChatResult{}, fmt.Errorf("llama: chat request carries no messages")
	}
	body, err := json.Marshal(toUpstream(s.cfg.ModelID, req, false))
	if err != nil {
		return gateway.ChatResult{}, err
	}
	resp, err := s.post(ctx, "/v1/chat/completions", body)
	if err != nil {
		return gateway.ChatResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return gateway.ChatResult{}, fmt.Errorf("llama: llama-server returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var ur upstreamResponse
	if err := json.NewDecoder(resp.Body).Decode(&ur); err != nil {
		return gateway.ChatResult{}, fmt.Errorf("llama: decode llama-server response: %w", err)
	}
	if len(ur.Choices) == 0 {
		return gateway.ChatResult{}, fmt.Errorf("llama: llama-server returned no choices")
	}
	return gateway.ChatResult{
		Content: ur.Choices[0].Message.Content,
		// Real counts from llama.cpp's own tokenizer.
		PromptTokens:     ur.Usage.PromptTokens,
		CompletionTokens: ur.Usage.CompletionTokens,
		Backend:          s.BackendName(),
		Nodes:            s.Nodes,
		FinishReason:     ur.Choices[0].FinishReason,
	}, nil
}

// ChatStream proxies a streaming completion, relaying each real token delta from
// llama-server's SSE as it arrives.
func (s *Service) ChatStream(ctx context.Context, _ string, req gateway.ChatRequest, emit func(delta string) error) (gateway.ChatResult, error) {
	if len(req.Messages) == 0 {
		return gateway.ChatResult{}, fmt.Errorf("llama: chat request carries no messages")
	}
	body, err := json.Marshal(toUpstream(s.cfg.ModelID, req, true))
	if err != nil {
		return gateway.ChatResult{}, err
	}
	resp, err := s.post(ctx, "/v1/chat/completions", body)
	if err != nil {
		return gateway.ChatResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return gateway.ChatResult{}, fmt.Errorf("llama: llama-server returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	res := gateway.ChatResult{Backend: s.BackendName(), Nodes: s.Nodes}
	var sb strings.Builder

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk upstreamResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Usage.PromptTokens > 0 {
			res.PromptTokens = chunk.Usage.PromptTokens
		}
		if chunk.Usage.CompletionTokens > 0 {
			res.CompletionTokens = chunk.Usage.CompletionTokens
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != "" {
				res.FinishReason = c.FinishReason
			}
			if c.Delta.Content == "" {
				continue
			}
			sb.WriteString(c.Delta.Content)
			if err := emit(c.Delta.Content); err != nil {
				res.Content = sb.String()
				return res, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		res.Content = sb.String()
		return res, fmt.Errorf("llama: read llama-server stream: %w", err)
	}
	res.Content = sb.String()
	return res, nil
}

func (s *Service) post(ctx context.Context, path string, body []byte) (*http.Response, error) {
	s.mu.Lock()
	base := s.baseURL
	started := s.started
	s.mu.Unlock()
	if !started {
		return nil, fmt.Errorf("llama: llama-server is not running")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// No client timeout: generation legitimately takes minutes. ctx governs.
	return (&http.Client{}).Do(req)
}

// ModelMeta describes this service's model for the gateway registry.
func (s *Service) ModelMeta() gateway.InferenceModelMeta {
	return gateway.InferenceModelMeta{
		Backend:   s.BackendName(),
		ModelPath: s.cfg.ModelPath,
	}
}

// ModelID is the id this service serves.
func (s *Service) ModelID() string { return s.cfg.ModelID }

var _ gateway.ChatBackend = (*Service)(nil)
