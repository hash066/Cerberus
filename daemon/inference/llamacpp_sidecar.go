//go:build windows || linux

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

//go:embed llamacpp/sidecar.py
var llamacppSidecarScript []byte

const llamacppProbeTimeout = 2 * time.Minute

type llamacppRequest struct {
	Op              string    `json:"op"`
	LayerLo         uint32    `json:"layer_lo,omitempty"`
	LayerHi         uint32    `json:"layer_hi,omitempty"`
	Activation      []float32 `json:"activation,omitempty"`
	ActivationBytes string    `json:"activation_bytes,omitempty"`
	Shape           []uint32  `json:"shape,omitempty"`
	DType           string    `json:"dtype,omitempty"`
	Hidden          []float32 `json:"hidden,omitempty"`
	Prompt          string    `json:"prompt,omitempty"`
	Model           string    `json:"model,omitempty"`
}

type llamacppResponse struct {
	OK              bool      `json:"ok"`
	Error           string    `json:"error,omitempty"`
	Backend         string    `json:"backend,omitempty"`
	Activation      []float32 `json:"activation,omitempty"`
	ActivationBytes string    `json:"activation_bytes,omitempty"`
	Shape           []uint32  `json:"shape,omitempty"`
	DType           string    `json:"dtype,omitempty"`
	Hidden          []float32 `json:"hidden,omitempty"`
	NLayers         int       `json:"n_layers,omitempty"`
	HiddenSize      int       `json:"hidden_size,omitempty"`
	LlamaCpp        bool      `json:"llama_cpp,omitempty"`
	Model           string    `json:"model,omitempty"`
}

type llamacppConn struct {
	mu sync.Mutex
	w  io.Writer
	r  *bufio.Reader
}

func newLlamacppConn(w io.Writer, r io.Reader) *llamacppConn {
	return &llamacppConn{w: w, r: bufio.NewReader(r)}
}

func (c *llamacppConn) roundTrip(req llamacppRequest) (llamacppResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, err := json.Marshal(req)
	if err != nil {
		return llamacppResponse{}, err
	}
	if _, err := c.w.Write(append(body, '\n')); err != nil {
		return llamacppResponse{}, fmt.Errorf("llamacpp: helper write: %w", err)
	}
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return llamacppResponse{}, fmt.Errorf("llamacpp: helper read: %w", err)
	}
	var resp llamacppResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return llamacppResponse{}, fmt.Errorf("llamacpp: decode helper response: %w", err)
	}
	return resp, nil
}

// LlamacppHelper owns one running helper process (Python sidecar or native binary).
type LlamacppHelper struct {
	cmd        *exec.Cmd
	conn       *llamacppConn
	stdin      io.WriteCloser
	scriptPath string
	native     bool
}

func startLlamacppHelper() (*LlamacppHelper, error) {
	if p := strings.TrimSpace(os.Getenv("CERBERUS_LLAMA_HELPER")); p != "" {
		return startLlamacppNativeHelper(p)
	}
	return startLlamacppPythonHelper()
}

func startLlamacppNativeHelper(path string) (*LlamacppHelper, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("llamacpp: CERBERUS_LLAMA_HELPER %q: %w", path, err)
	}
	cmd := exec.Command(path)
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("llamacpp: start native helper: %w", err)
	}
	h := &LlamacppHelper{cmd: cmd, conn: newLlamacppConn(stdin, stdout), stdin: stdin, native: true}
	ctx, cancel := context.WithTimeout(context.Background(), llamacppProbeTimeout)
	defer cancel()
	if _, err := h.call(ctx, llamacppRequest{Op: "ping"}); err != nil {
		h.Close()
		return nil, fmt.Errorf("llamacpp: native helper did not answer ping: %w", err)
	}
	return h, nil
}

func startLlamacppPythonHelper() (*LlamacppHelper, error) {
	python, err := resolveLlamacppPython()
	if err != nil {
		return nil, err
	}
	scriptPath, err := writeLlamacppSidecarScript()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(python, scriptPath)
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
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
		return nil, fmt.Errorf("llamacpp: start helper (%s): %w", python, err)
	}
	h := &LlamacppHelper{cmd: cmd, conn: newLlamacppConn(stdin, stdout), stdin: stdin, scriptPath: scriptPath}
	ctx, cancel := context.WithTimeout(context.Background(), llamacppProbeTimeout)
	defer cancel()
	if _, err := h.call(ctx, llamacppRequest{Op: "ping"}); err != nil {
		h.Close()
		return nil, fmt.Errorf("llamacpp: helper did not answer ping: %w", err)
	}
	return h, nil
}

func (h *LlamacppHelper) call(ctx context.Context, req llamacppRequest) (llamacppResponse, error) {
	type outcome struct {
		resp llamacppResponse
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := h.conn.roundTrip(req)
		done <- outcome{resp, err}
	}()
	select {
	case o := <-done:
		return o.resp, o.err
	case <-ctx.Done():
		h.Close()
		return llamacppResponse{}, fmt.Errorf("llamacpp: helper %s: %w", req.Op, ctx.Err())
	}
}

func (h *LlamacppHelper) Close() {
	if h.stdin != nil {
		_ = h.stdin.Close()
	}
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
		_ = h.cmd.Wait()
	}
	if h.scriptPath != "" {
		_ = os.Remove(h.scriptPath)
	}
}

func resolveLlamacppPython() (string, error) {
	if p := strings.TrimSpace(os.Getenv("CERBERUS_LLAMA_PYTHON")); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("llamacpp: CERBERUS_LLAMA_PYTHON %q: %w", p, err)
		}
		return p, nil
	}
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("llamacpp: no python interpreter found (install python3 or set CERBERUS_LLAMA_PYTHON)")
}

func writeLlamacppSidecarScript() (string, error) {
	f, err := os.CreateTemp("", "cerberus-llama-sidecar-*.py")
	if err != nil {
		return "", fmt.Errorf("llamacpp: temp sidecar script: %w", err)
	}
	if _, err := f.Write(llamacppSidecarScript); err != nil {
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

var (
	llamacppMu       sync.Mutex
	llamacppHelper   *LlamacppHelper
	llamacppProbed   bool
	llamacppProbeErr error
	llamacppHasReal  bool
)

func sharedLlamacppHelper() (*LlamacppHelper, error) {
	llamacppMu.Lock()
	defer llamacppMu.Unlock()
	probeLlamacppLocked()
	if llamacppProbeErr != nil {
		return nil, llamacppProbeErr
	}
	return llamacppHelper, nil
}

func probeLlamacppLocked() {
	if llamacppProbed {
		return
	}
	llamacppProbed = true
	if mockForced() {
		llamacppProbeErr = fmt.Errorf("llamacpp: mock mode enabled")
		return
	}
	h, err := startLlamacppHelper()
	if err != nil {
		llamacppProbeErr = err
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), llamacppProbeTimeout)
	defer cancel()
	info, err := h.call(ctx, llamacppRequest{Op: "info"})
	if err != nil {
		h.Close()
		llamacppProbeErr = err
		return
	}
	llamacppHelper = h
	llamacppHasReal = info.LlamaCpp || h.native
}

// LlamacppHelperReady reports whether model+helper are available for real forward.
func LlamacppHelperReady() bool {
	if !llamacppSupported() || mockForced() {
		return false
	}
	llamacppMu.Lock()
	defer llamacppMu.Unlock()
	probeLlamacppLocked()
	return llamacppProbeErr == nil && llamacppHasReal
}

// LlamacppReportedBackend is the honest PipelineResult backend string.
func LlamacppReportedBackend() string {
	if mockForced() || !llamacppSupported() {
		return llamacppMockBackend
	}
	if !LlamacppHelperReady() {
		return llamacppMockBackend
	}
	if !llamaModelConfigured() {
		return llamacppMockBackend
	}
	return llamacppRealBackend
}

// HelperPathForTest returns the resolved helper path for diagnostics.
func HelperPathForTest() (string, error) {
	if p := strings.TrimSpace(os.Getenv("CERBERUS_LLAMA_HELPER")); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", err
		}
		return p, nil
	}
	return resolveLlamacppPython()
}
