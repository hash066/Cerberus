// mlx_sidecar.go is the Go side of the MLX Python sidecar (mlx/sidecar.py):
// it spawns the script under a Python interpreter and exchanges one JSON
// object per line over stdin/stdout. The protocol client (mlxConn) is
// transport-agnostic so tests exercise it over in-memory pipes without
// Python; process management is the thin layer on top.
//
// The sidecar only genuinely computes on macOS/Apple Silicon with the mlx
// pip package installed (see mlx/README.md). Everywhere else the probe fails
// fast and callers fall back to the mock/clear-error paths in mlx_engine.go.
package inference

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed mlx/sidecar.py
var mlxSidecarScript []byte

// mlxProbeTimeout bounds the ping+info handshake at sidecar start (a cold
// Python interpreter import of mlx can take a few seconds).
const mlxProbeTimeout = 30 * time.Second

// mlxRequest is one request line to the sidecar.
type mlxRequest struct {
	Op              string    `json:"op"`
	LayerLo         uint32    `json:"layer_lo,omitempty"`
	LayerHi         uint32    `json:"layer_hi,omitempty"`
	Activation      []float32 `json:"activation,omitempty"`
	ActivationBytes string    `json:"activation_bytes,omitempty"`
	Shape           []uint32  `json:"shape,omitempty"`
	DType           string    `json:"dtype,omitempty"`
	Tokens          []int     `json:"tokens,omitempty"`
	Prompt          string    `json:"prompt,omitempty"`
	MaxTokens       int       `json:"max_tokens,omitempty"`
	Model           string    `json:"model,omitempty"`
}

// MLXTopToken is one entry of the last-position logits top-k.
type MLXTopToken struct {
	Token int     `json:"token"`
	Logit float64 `json:"logit"`
}

// mlxResponse is one response line from the sidecar (union of all ops).
type mlxResponse struct {
	OK              bool          `json:"ok"`
	Error           string        `json:"error,omitempty"`
	Backend         string        `json:"backend,omitempty"`
	Activation      []float32     `json:"activation,omitempty"`
	ActivationBytes string        `json:"activation_bytes,omitempty"`
	Shape           []uint32      `json:"shape,omitempty"`
	DType           string        `json:"dtype,omitempty"`
	Text            string        `json:"text,omitempty"`
	Tokens          []int         `json:"tokens,omitempty"`
	TopTokens       []MLXTopToken `json:"top_tokens,omitempty"`
	NLayers         int           `json:"n_layers,omitempty"`
	HiddenSize      int           `json:"hidden_size,omitempty"`
	// info fields
	MLX      bool   `json:"mlx,omitempty"`
	MLXLM    bool   `json:"mlx_lm,omitempty"`
	Platform string `json:"platform,omitempty"`
	Model    string `json:"model,omitempty"`
}

// mlxConn serializes JSON-lines request/response over any byte stream pair.
// Requests are answered strictly in order, so one mutex is the whole story.
type mlxConn struct {
	mu sync.Mutex
	w  io.Writer
	r  *bufio.Reader
}

func newMLXConn(w io.Writer, r io.Reader) *mlxConn {
	return &mlxConn{w: w, r: bufio.NewReader(r)}
}

// roundTrip sends one request and blocks for its response line.
func (c *mlxConn) roundTrip(req mlxRequest) (mlxResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, err := json.Marshal(req)
	if err != nil {
		return mlxResponse{}, err
	}
	if _, err := c.w.Write(append(body, '\n')); err != nil {
		return mlxResponse{}, fmt.Errorf("mlx: sidecar write: %w", err)
	}
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return mlxResponse{}, fmt.Errorf("mlx: sidecar read: %w", err)
	}
	var resp mlxResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return mlxResponse{}, fmt.Errorf("mlx: decode sidecar response: %w", err)
	}
	return resp, nil
}

// MLXSidecar owns one running sidecar process.
type MLXSidecar struct {
	cmd        *exec.Cmd
	conn       *mlxConn
	stdin      io.WriteCloser
	scriptPath string // temp copy of the embedded script, removed on Close
}

// startMLXSidecar spawns python + the embedded script and verifies liveness
// with a ping. It does NOT require mlx to be importable — op "info" reports
// that honestly and the caller decides.
func startMLXSidecar() (*MLXSidecar, error) {
	python, err := resolveMLXPython()
	if err != nil {
		return nil, err
	}
	scriptPath, err := writeMLXSidecarScript()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(python, scriptPath)
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr // sidecar diagnostics stay visible, never on the protocol stream
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = os.Remove(scriptPath)
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = os.Remove(scriptPath)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(scriptPath)
		return nil, fmt.Errorf("mlx: start sidecar (%s): %w", python, err)
	}
	sc := &MLXSidecar{cmd: cmd, conn: newMLXConn(stdin, stdout), stdin: stdin, scriptPath: scriptPath}

	ctx, cancel := context.WithTimeout(context.Background(), mlxProbeTimeout)
	defer cancel()
	if _, err := sc.call(ctx, mlxRequest{Op: "ping"}); err != nil {
		sc.Close()
		return nil, fmt.Errorf("mlx: sidecar did not answer ping: %w", err)
	}
	return sc, nil
}

// call performs a round trip, aborting (and killing the sidecar — the stream
// would be desynchronized) if ctx expires first.
func (s *MLXSidecar) call(ctx context.Context, req mlxRequest) (mlxResponse, error) {
	type outcome struct {
		resp mlxResponse
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := s.conn.roundTrip(req)
		done <- outcome{resp, err}
	}()
	select {
	case o := <-done:
		return o.resp, o.err
	case <-ctx.Done():
		s.Close()
		return mlxResponse{}, fmt.Errorf("mlx: sidecar %s: %w", req.Op, ctx.Err())
	}
}

// Close terminates the sidecar process and removes the temp script.
func (s *MLXSidecar) Close() {
	if s.stdin != nil {
		_ = s.stdin.Close() // EOF on stdin ends the sidecar main loop
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
	if s.scriptPath != "" {
		_ = os.Remove(s.scriptPath)
	}
}

// resolveMLXPython picks the interpreter: CERBERUS_MLX_PYTHON, else python3,
// else python on PATH.
func resolveMLXPython() (string, error) {
	if p := strings.TrimSpace(os.Getenv("CERBERUS_MLX_PYTHON")); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("mlx: CERBERUS_MLX_PYTHON %q: %w", p, err)
		}
		return p, nil
	}
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("mlx: no python interpreter found (install python3 or set CERBERUS_MLX_PYTHON)")
}

// writeMLXSidecarScript materializes the embedded sidecar script so a shipped
// daemon binary needs no source checkout.
func writeMLXSidecarScript() (string, error) {
	f, err := os.CreateTemp("", "cerberus-mlx-sidecar-*.py")
	if err != nil {
		return "", fmt.Errorf("mlx: temp sidecar script: %w", err)
	}
	if _, err := f.Write(mlxSidecarScript); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return filepath.Clean(f.Name()), nil
}
