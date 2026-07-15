// Command e2e is the v0.1 acceptance demo: it builds cerberusd, spawns two
// daemon processes, waits for peer discovery, dispatches hello-shard.wasm from
// one node to the other, and asserts the returned value.
//
// Run: go run ./test/e2e   (or `task demo`)
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
	"strings"
	"sync"
	"time"

	"github.com/hash066/cerberus/test/e2e/node"
	"github.com/hash066/cerberus/test/testdaemon"
)

const (
	requesterID = "requester"
	workerID    = "worker"
	timeout     = 20 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "DEMO FAILED:", err)
		os.Exit(1)
	}
	fmt.Printf("DEMO PASSED: %s ran remotely on %s and returned %d.\n", "hello-shard.wasm", workerID, node.HelloShardValue)
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	repoRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	workDir, err := os.MkdirTemp(repoRoot, ".cerberus-e2e-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	binaryPath, err := testdaemon.BuildCerberusd(repoRoot)
	if err != nil {
		return err
	}
	wasmPath := filepath.Join(workDir, "hello-shard.wasm")
	if err := os.WriteFile(wasmPath, node.HelloShardWASM(), 0o644); err != nil {
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

	result, err := dispatchRemote(ctx, requester.ready.Addr, node.DispatchRequest{
		TargetID: workerID,
		WasmPath: wasmPath,
	})
	if err != nil {
		return attachLogs("remote dispatch failed", err, requester, worker)
	}
	if !result.OK {
		return attachLogs("remote execution returned failure", errors.New(result.Error), requester, worker)
	}
	if result.PeerID != workerID {
		return attachLogs("remote execution ran on wrong peer", fmt.Errorf("got %q want %q", result.PeerID, workerID), requester, worker)
	}
	if result.Value != node.HelloShardValue {
		return attachLogs("unexpected hello-shard return value", fmt.Errorf("got %d want %d", result.Value, node.HelloShardValue), requester, worker)
	}
	fmt.Printf("[harness] remote hello-shard returned %d from %s\n", result.Value, result.PeerID)
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
	args := []string{"-e2e-node", "-e2e-id", id, "-e2e-listen", "127.0.0.1:0"}
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
	case <-time.After(5 * time.Second):
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
	if err := scanner.Err(); err != nil {
		logs.WriteString(fmt.Sprintf("[%s %s] scanner error: %v\n", id, stream, err))
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

func dispatchRemote(ctx context.Context, requesterURL string, req node.DispatchRequest) (node.RunResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return node.RunResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requesterURL+"/dispatch", bytes.NewReader(body))
	if err != nil {
		return node.RunResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return node.RunResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return node.RunResponse{}, fmt.Errorf("dispatch returned %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	var result node.RunResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return node.RunResponse{}, err
	}
	return result, nil
}

func attachLogs(message string, cause error, procs ...*daemonProc) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %v", message, cause)
	for _, proc := range procs {
		fmt.Fprintf(&b, "\n--- %s logs ---\n%s", proc.id, proc.logs.String())
	}
	return errors.New(b.String())
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
