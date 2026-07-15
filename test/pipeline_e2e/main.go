// Package pipeline_e2e runs the two-node layer-split pipeline demo: two
// cerberusd -e2e-node processes discover each other, then the requester
// orchestrates a 4-layer split-MLP across both nodes via PipelineRunner.
//
// Run: go run ./test/pipeline_e2e
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hash066/cerberus/daemon/system"
	"github.com/hash066/cerberus/test/e2e/node"
)

const (
	requesterID = "requester"
	workerID    = "worker"
	timeout     = 90 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "PIPELINE DEMO FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("PIPELINE DEMO PASSED: 4-layer split-MLP ran across two nodes.")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	repoRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	workDir, err := os.MkdirTemp(repoRoot, ".cerberus-pipeline-e2e-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	binaryPath := filepath.Join(workDir, executableName("cerberusd"))
	if err := buildDaemon(ctx, repoRoot, binaryPath); err != nil {
		return err
	}

	requester, err := startDaemon(ctx, repoRoot, binaryPath, requesterID, nil)
	if err != nil {
		return err
	}
	defer requester.stop()
	worker, err := startDaemon(ctx, repoRoot, binaryPath, workerID, []string{requester.ready.Addr})
	if err != nil {
		return err
	}
	defer worker.stop()

	fmt.Printf("[harness] spawned %s at %s\n", requesterID, requester.ready.Addr)
	fmt.Printf("[harness] spawned %s at %s\n", workerID, worker.ready.Addr)

	if err := waitForDiscovery(ctx, requester.ready.Addr, workerID); err != nil {
		return attachLogs("requester did not discover worker", err, requester, worker)
	}
	if err := waitForDiscovery(ctx, worker.ready.Addr, requesterID); err != nil {
		return attachLogs("worker did not discover requester", err, requester, worker)
	}
	fmt.Println("[harness] nodes discovered each other")

	result, err := pipelineRun(ctx, requester.ready.Addr)
	if err != nil {
		return attachLogs("pipeline run failed", err, requester, worker)
	}
	if !result.OK {
		return attachLogs("pipeline returned failure", errors.New(result.Error), requester, worker)
	}

	want := system.EncodeActivation((system.SplitMLP{}).ExpectedOutput())
	if !bytes.Equal(result.Output, want) {
		return attachLogs("unexpected pipeline output", fmt.Errorf("got %x want %x", result.Output, want), requester, worker)
	}
	if len(result.Stages) < 2 {
		return attachLogs("expected at least 2 stages", fmt.Errorf("got %d", len(result.Stages)), requester, worker)
	}
	fmt.Printf("[harness] pipeline output matches reference (%d stages)\n", len(result.Stages))
	return nil
}

type pipelineRunResult struct {
	OK      bool     `json:"ok"`
	Output  []byte   `json:"output"`
	Error   string   `json:"error,omitempty"`
	Backend string   `json:"backend"`
	Stages  []string `json:"stages,omitempty"`
}

func pipelineRun(ctx context.Context, requesterURL string) (pipelineRunResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requesterURL+"/pipeline-run", nil)
	if err != nil {
		return pipelineRunResult{}, err
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return pipelineRunResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return pipelineRunResult{}, fmt.Errorf("pipeline-run returned %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	var result pipelineRunResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return pipelineRunResult{}, err
	}
	return result, nil
}

func buildDaemon(ctx context.Context, repoRoot, binaryPath string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binaryPath, "./cmd/cerberusd")
	cmd.Dir = repoRoot
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build cerberusd: %w\n%s", err, strings.TrimSpace(output.String()))
	}
	fmt.Printf("[harness] built %s\n", binaryPath)
	return nil
}

type daemonProc struct {
	id     string
	cancel context.CancelFunc
	ready  node.ReadyMessage
	wait   <-chan error
	logs   *safeBuffer
}

func startDaemon(parent context.Context, repoRoot, binaryPath, id string, peers []string) (*daemonProc, error) {
	ctx, cancel := context.WithCancel(parent)
	args := []string{"-e2e-node", "-e2e-id", id, "-e2e-listen", "127.0.0.1:0", "-e2e-pipeline"}
	if len(peers) > 0 {
		args = append(args, "-e2e-peer", strings.Join(peers, ","))
	}
	cmd := exec.CommandContext(ctx, binaryPath, args...)
	cmd.Dir = repoRoot

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	logs := &safeBuffer{}
	readyCh := make(chan node.ReadyMessage, 1)
	go scanOutput(id, "stdout", stdout, logs, readyCh)
	go scanOutput(id, "stderr", stderr, logs, nil)
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case ready := <-readyCh:
		return &daemonProc{id: id, cancel: cancel, ready: ready, wait: waitCh, logs: logs}, nil
	case err := <-waitCh:
		cancel()
		return nil, fmt.Errorf("%s exited before ready: %w\n%s", id, err, logs.String())
	case <-parent.Done():
		cancel()
		return nil, parent.Err()
	case <-time.After(8 * time.Second):
		cancel()
		return nil, fmt.Errorf("timed out waiting for %s readiness\n%s", id, logs.String())
	}
}

func (p *daemonProc) stop() {
	p.cancel()
	select {
	case <-p.wait:
	case <-time.After(2 * time.Second):
	}
}

func scanOutput(id, stream string, reader io.Reader, logs *safeBuffer, readyCh chan<- node.ReadyMessage) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		logs.WriteString(fmt.Sprintf("[%s %s] %s\n", id, stream, line))
		if readyCh != nil && strings.HasPrefix(line, node.ReadyPrefix) {
			var ready node.ReadyMessage
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, node.ReadyPrefix)), &ready); err == nil {
				readyCh <- ready
			}
		}
	}
}

func waitForDiscovery(ctx context.Context, baseURL, peerID string) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if hasPeer(ctx, client, baseURL, peerID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func hasPeer(ctx context.Context, client *http.Client, baseURL, peerID string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/peers", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var peers node.PeersResponse
	if err := json.NewDecoder(resp.Body).Decode(&peers); err != nil {
		return false
	}
	for _, peer := range peers.Peers {
		if peer.ID == peerID {
			return true
		}
	}
	return false
}

func attachLogs(message string, cause error, procs ...*daemonProc) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %v", message, cause)
	for _, proc := range procs {
		fmt.Fprintf(&b, "\n--- %s logs ---\n%s", proc.id, proc.logs.String())
	}
	return errors.New(b.String())
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *safeBuffer) WriteString(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.b.WriteString(s)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
