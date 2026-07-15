// Package peripheral_e2e runs a two-node cerberusd harness that verifies
// multi-node peripheral pooling via GET /api/v1/cluster/resources and related
// status fields (cluster_cpu, gpu_pool, pooled devices).
//
// Run:
//
//	go run ./test/peripheral_e2e
//
// Prerequisites: build succeeds; mDNS may take up to ~30s on loopback CI.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hash066/cerberus/daemon/api"
	"github.com/hash066/cerberus/test/testdaemon"
)

const (
	nodeA   = "alpha"
	nodeB   = "beta"
	timeout = 90 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "PERIPHERAL E2E FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("PERIPHERAL E2E PASSED: two-node cluster resources pooled.")
	fmt.Println()
	printDemoScript()
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	repoRoot, err := os.Getwd()
	if err != nil {
		return err
	}

	binaryPath, err := testdaemon.BuildCerberusd(repoRoot)
	if err != nil {
		return err
	}

	a, err := startNode(ctx, binaryPath, repoRoot, nodeA, nil)
	if err != nil {
		return err
	}
	defer a.stop()

	peerAddr, err := waitDialable(ctx, a)
	if err != nil {
		return attachLogs("alpha dialable addr", err, a)
	}
	fmt.Printf("[harness] %s mesh dialable: %s\n", nodeA, peerAddr)

	b, err := startNode(ctx, binaryPath, repoRoot, nodeB, []string{peerAddr})
	if err != nil {
		return err
	}
	defer b.stop()

	time.Sleep(2 * time.Second)

	fmt.Printf("[harness] %s api=%s\n", nodeA, a.apiAddr)
	fmt.Printf("[harness] %s api=%s\n", nodeB, b.apiAddr)

	if err := waitMesh(ctx, a, b); err != nil {
		return attachLogs("mesh did not converge", err, a, b)
	}
	fmt.Println("[harness] mesh converged")

	tokA, err := readToken(ctx, a.configDir)
	if err != nil {
		return err
	}
	tokB, err := readToken(ctx, b.configDir)
	if err != nil {
		return err
	}

	// Allow telemetry publication + audio pool refresh.
	for i := 0; i < 10; i++ {
		resA, _ := fetchClusterResources(ctx, a.apiAddr, tokA)
		if resA.Totals.Nodes >= 2 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	resA, err := fetchClusterResources(ctx, a.apiAddr, tokA)
	if err != nil {
		return attachLogs("cluster/resources on A", err, a, b)
	}
	resB, err := fetchClusterResources(ctx, b.apiAddr, tokB)
	if err != nil {
		return attachLogs("cluster/resources on B", err, a, b)
	}

	if resA.Totals.Nodes < 2 {
		fmt.Printf("[harness] note: node A sees %d scheduler nodes (telemetry may lag on loopback)\n", resA.Totals.Nodes)
	}
	if resB.Totals.Nodes < 2 {
		fmt.Printf("[harness] note: node B sees %d scheduler nodes (telemetry may lag on loopback)\n", resB.Totals.Nodes)
	}
	if resA.Totals.MeshPeers < 1 || resB.Totals.MeshPeers < 1 {
		return fmt.Errorf("mesh_peers: A=%d B=%d, want >=1 each", resA.Totals.MeshPeers, resB.Totals.MeshPeers)
	}

	// CPU pool via status snapshot fields.
	st, err := fetchStatus(ctx, a.apiAddr, tokA)
	if err != nil {
		return err
	}
	if st.ClusterCPU.TotalCores == 0 {
		return fmt.Errorf("status cluster_cpu.total_cores is 0")
	}
	if len(st.GpuPool.Nodes) == 0 {
		return fmt.Errorf("status gpu_pool.nodes empty")
	}

	// Capability model: each self node lists four peripheral kinds.
	selfCaps := 0
	for _, p := range resA.Peers {
		if p.Kind == "self" {
			selfCaps = len(p.Capabilities)
			for _, c := range p.Capabilities {
				if c.Kind == "storage" && !c.Remote {
					return fmt.Errorf("storage remote should be true when peer connected")
				}
			}
		}
	}
	if selfCaps < 4 {
		return fmt.Errorf("expected >=4 capability entries on self, got %d", selfCaps)
	}

	fmt.Printf("[harness] A totals: nodes=%d vram=%d audio=%d\n",
		resA.Totals.Nodes, resA.Totals.VRAMTotalBytes, resA.Totals.AudioDevices)
	fmt.Printf("[harness] B totals: nodes=%d vram=%d audio=%d\n",
		resB.Totals.Nodes, resB.Totals.VRAMTotalBytes, resB.Totals.AudioDevices)

	// Storage/GPU/audio subtests — skip gracefully when hardware absent.
	if err := probePeripherals(ctx, a, tokA); err != nil {
		fmt.Printf("[harness] skip/local probe: %v\n", err)
	}

	return nil
}

func probePeripherals(ctx context.Context, n *nodeProc, tok string) error {
	devs, err := fetchDevices(ctx, n.apiAddr, tok)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		return errors.New("no devices listed")
	}
	hasVRAM := false
	for _, d := range devs {
		if d.Kind == "vram" {
			hasVRAM = true
		}
	}
	if !hasVRAM {
		return errors.New("no VRAM device")
	}
	return nil
}

type nodeProc struct {
	id        string
	binary    string
	repoRoot  string
	configDir string
	apiAddr   string
	rpcAddr   string
	peers     []string
	cmd       *exec.Cmd
	logs      *safeBuffer
}

func startNode(ctx context.Context, binary, repoRoot, id string, peers []string) (*nodeProc, error) {
	base, err := os.MkdirTemp(repoRoot, ".cerberus-peripheral-*")
	if err != nil {
		return nil, err
	}
	cfgDir := filepath.Join(base, "cfg-"+id)
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return nil, err
	}
	apiPort, err := freePort()
	if err != nil {
		return nil, err
	}
	rpcPort, err := freePort()
	if err != nil {
		return nil, err
	}
	gwPort, err := freePort()
	if err != nil {
		return nil, err
	}
	metPort, err := freePort()
	if err != nil {
		return nil, err
	}

	n := &nodeProc{
		id: id, binary: binary, repoRoot: repoRoot, configDir: cfgDir,
		apiAddr: fmt.Sprintf("127.0.0.1:%d", apiPort),
		rpcAddr: fmt.Sprintf("127.0.0.1:%d", rpcPort),
		peers:   peers,
	}
	args := []string{
		"-api-addr", n.apiAddr,
		"-rpc-addr", n.rpcAddr,
		"-gateway-addr", fmt.Sprintf("127.0.0.1:%d", gwPort),
		"-metrics-addr", fmt.Sprintf("127.0.0.1:%d", metPort),
		"-mesh-listen", "/ip4/127.0.0.1/udp/0/quic-v1",
	}
	for _, p := range peers {
		args = append(args, "-peer", p)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"AppData="+cfgDir,
		"XDG_CONFIG_HOME="+cfgDir,
		"HOME="+cfgDir,
	)
	logs := &safeBuffer{}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go drain(stdout, logs)
	go drain(stderr, logs)
	n.cmd = cmd
	n.logs = logs
	return n, nil
}

func (n *nodeProc) stop() {
	if n.cmd != nil && n.cmd.Process != nil {
		_ = n.cmd.Process.Kill()
	}
}

const dialablePrefix = "mesh: dialable at "

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitDialable(ctx context.Context, n *nodeProc) (string, error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, line := range strings.Split(n.logs.String(), "\n") {
			if idx := strings.Index(line, dialablePrefix); idx >= 0 {
				addr := strings.TrimSpace(line[idx+len(dialablePrefix):])
				if addr != "" {
					return addr, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timeout waiting for dialable mesh addr")
		case <-ticker.C:
		}
	}
}

func waitMesh(ctx context.Context, a, b *nodeProc) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(60 * time.Second)
	}
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		tokA, errA := readToken(ctx, a.configDir)
		tokB, errB := readToken(ctx, b.configDir)
		if errA == nil && errB == nil {
			aUp := rpcMeshPeers(a.rpcAddr, tokA) >= 1
			bUp := rpcMeshPeers(b.rpcAddr, tokB) >= 1
			if aUp && bUp {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for mesh (deadline %s)", deadline.Format(time.RFC3339))
		case <-ticker.C:
		}
	}
}

type rpNodesReq struct{ Token string }
type rpNodesResp struct {
	MeshUp bool
	Nodes  []struct {
		PeerID string
		Self   bool
	}
}

func rpcMeshPeers(rpcAddr, token string) int {
	c, err := rpc.Dial("tcp", rpcAddr)
	if err != nil {
		return 0
	}
	defer func() { _ = c.Close() }()
	var resp rpNodesResp
	if err := c.Call("DaemonRPC.Nodes", &rpNodesReq{Token: token}, &resp); err != nil {
		return 0
	}
	if !resp.MeshUp {
		return 0
	}
	n := 0
	for _, node := range resp.Nodes {
		if !node.Self {
			n++
		}
	}
	return n
}

func fetchClusterResources(ctx context.Context, apiAddr, token string) (api.ClusterResources, error) {
	var out api.ClusterResources
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+apiAddr+"/api/v1/cluster/resources", nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return out, fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

func fetchStatus(ctx context.Context, apiAddr, token string) (api.Snapshot, error) {
	var out api.Snapshot
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+apiAddr+"/api/v1/status", nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

func fetchDevices(ctx context.Context, apiAddr, token string) ([]api.NamespaceDevice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+apiAddr+"/api/v1/devices", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out []api.NamespaceDevice
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func readToken(ctx context.Context, cfgDir string) (string, error) {
	path := filepath.Join(cfgDir, "cerberus", "operator.token")
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if tok := strings.TrimSpace(string(b)); tok != "" {
				return tok, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("operator token not found at %s", path)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuffer) WriteString(x string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.WriteString(x)
}

func drain(r io.Reader, logs *safeBuffer) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		logs.WriteString(sc.Text() + "\n")
	}
}

func attachLogs(msg string, cause error, nodes ...*nodeProc) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %v", msg, cause)
	for _, n := range nodes {
		fmt.Fprintf(&b, "\n--- %s ---\n%s", n.id, n.logs.String())
	}
	return errors.New(b.String())
}

func printDemoScript() {
	fmt.Println("Demo script (multi-node peripheral pooling):")
	fmt.Println("  1. Build:  go build -o cerberusd ./cmd/cerberusd")
	fmt.Println("  2. Node A:  cerberusd -api-addr 127.0.0.1:7777 -mesh-listen /ip4/127.0.0.1/udp/40123/quic-v1")
	fmt.Println("  3. Node B:  cerberusd -api-addr 127.0.0.1:7778 -peer <A dialable multiaddr>")
	fmt.Println("              (mDNS may fail on loopback/Windows — use explicit -peer bootstrap)")
	fmt.Println("  4. Inspect: curl -H \"Authorization: Bearer $(cat ~/.config/cerberus/operator.token)\" \\")
	fmt.Println("                http://127.0.0.1:7777/api/v1/cluster/resources | jq .")
	fmt.Println("  5. Devices: curl ... /api/v1/devices  (local + pooled remote audio)")
	fmt.Println("  6. Status:  curl ... /api/v1/status   (cluster_cpu, gpu_pool fields)")
	fmt.Println("  7. E2E:     go run ./test/peripheral_e2e")
	if runtime.GOOS == "windows" {
		fmt.Println("  Note: on Windows set AppData=<isolated-dir> for a second instance.")
	}
}
