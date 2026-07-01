// Package chaos: this file is the REAL-PROCESS counterpart to the in-process
// simulated suites in partition_test.go / nodeloss_test.go / liddrop_test.go.
//
// Everything else in test/chaos runs against contract/go/stub.CapKernel (an
// in-memory fake kernel) and an in-process simulated nodeFabric/busBroker (see
// cluster.go): real production packages (daemon/scheduler, daemon/lifecycle,
// daemon/auth, daemon/state) are exercised directly as libraries, but the
// "network" is a Go channel fan-out the test can Partition()/Heal() at will, and
// there is no real OS process, no real socket, no real scheduler jitter. That
// proves the CONVERGENCE ALGORITHMS are correct under a simulated fault; it says
// nothing about how the actually-deployed daemon (real `cerberusd` processes,
// real TCP/QUIC sockets, real mDNS multicast, real OS process scheduling)
// behaves when something actually fails.
//
// This file closes that gap for the one fault class available without admin
// rights: a real OS process KILL + RESTART (a node genuinely crashing
// mid-operation and later rejoining the mesh), as opposed to a network-level
// partition. Firewall/netsh-based network partitioning is intentionally NOT
// attempted here — it needs elevated privileges this environment does not have
// and is out of scope for this suite; see docs/verticals for that follow-up.
//
// Model, reusing test/e2e's pattern (build once, exec multiple real
// `cerberusd` instances, poll over the real control-plane RPC rather than
// inspecting in-process state):
//
//   - Two real `cerberusd` processes (not the -e2e-node demo mode: the FULL
//     daemon path — system.Compose's real libp2p/QUIC mesh with mDNS enabled,
//     the real durable bbolt-backed auth.Issuer + RevocationGossip over the
//     real "sys/revocations" topic, and the real net/rpc DaemonRPC control
//     surface cmd/cerberus talks to) are spawned as separate OS processes with
//     isolated per-node config directories (via AppData/XDG_CONFIG_HOME
//     overrides — cerberusd has no --data-dir flag, so this is the only way to
//     run two instances on one box without them colliding on the same bbolt
//     file) and distinct RPC/gateway/api/metrics ports.
//   - The harness waits for REAL mDNS discovery to converge (both processes'
//     Nodes RPC reporting the other as a connected mesh peer) — genuine network
//     activity, not a simulated bus.
//   - The fault: one node is os.Process.Kill()'d — a hard, immediate OS-level
//     kill (no graceful shutdown), exactly what a real crash looks like from
//     the outside — while a capability revocation happens elsewhere in the
//     mesh. The node is then relaunched as a fresh process with the same
//     config dir (so the same durable issuer key / bbolt store), simulating a
//     crash-and-restart with preserved identity.
//   - The post-condition asserted is the SAME kind the simulated
//     TestPartitionThenHealConvergesRevocation checks — a revocation that could
//     not reach a node while it was unreachable/down converges (is enforced)
//     once that node rejoins the mesh and the origin re-announces — but here it
//     is observed via REAL RPC calls (DaemonRPC.CapsMint/CapsRevoke/CapsList)
//     against REAL processes, not by inspecting an in-process *auth.Issuer.
//
// These tests build a real Go binary and spin up real OS processes with real
// network discovery, so they are inherently much slower than the in-process
// suites (seconds, not milliseconds) and are gated behind testing.Short(),
// mirroring the networked-mesh tests in test/e2e/node/mesh_compute_test.go and
// test/load/load_test.go.
package chaos

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- RPC wire types --------------------------------------------------------
//
// net/rpc (gob) matches by field name/type, not by importing cmd/cerberusd
// (package main, unimportable). These mirror cmd/cerberusd/rpc.go and
// cmd/cerberus/rpctypes.go exactly, the same way rpctypes.go itself documents
// duplicating rather than importing.

type rpNodesRequest struct{ Token string }
type rpNodeEntry struct {
	PeerID string
	Addr   string
	Self   bool
}
type rpNodesResponse struct {
	SelfPeer string
	Nodes    []rpNodeEntry
	MeshUp   bool
}

type rpCapsMintRequest struct {
	Token    string
	Subject  string
	Rights   []string
	Resource string
	TTLSecs  int64
}
type rpCapsMintResponse struct {
	Token   string
	ID      string
	Subject string
}

type rpCapsRevokeRequest struct {
	Token string
	ID    string
}
type rpCapsRevokeResponse struct {
	ID      string
	Revoked bool
}

type rpCapsListRequest struct{ Token string }
type rpCapEntry struct {
	ID       string
	Subject  string
	Rights   []string
	Resource string
	Expiry   int64
	Parent   string
	Revoked  bool
}
type rpCapsListResponse struct {
	Caps []rpCapEntry
}

// ---- real-process harness --------------------------------------------------

// realNode is one real `cerberusd` OS process plus everything the test needs
// to talk to it and restart it identically. Unlike the in-process `node` type
// in cluster.go, there is no shared Go object graph here at all — every
// interaction crosses a real TCP socket.
type realNode struct {
	id          string
	binaryPath  string
	repoRoot    string
	configDir   string // isolated AppData/XDG_CONFIG_HOME equivalent; survives restart
	rpcAddr     string
	gwAddr      string
	apiAddr     string
	metricsAddr string

	mu      sync.Mutex
	cmd     *exec.Cmd
	logs    *rpSafeBuffer
	waitErr chan error
}

// rpSafeBuffer is a concurrency-safe log sink, mirroring test/e2e/main.go's
// safeBuffer (duplicated rather than imported: that type is unexported in
// package main).
type rpSafeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *rpSafeBuffer) WriteString(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.b.WriteString(s)
}
func (b *rpSafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// buildCerberusd builds the real daemon binary once, exactly like
// test/e2e/main.go's buildDaemon, so both real-process nodes exec the same
// tested artifact rather than `go run`-ing per node.
func buildCerberusd(t *testing.T, repoRoot, outDir string) string {
	t.Helper()
	binPath := filepath.Join(outDir, executableName("cerberusd"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./cmd/cerberusd")
	cmd.Dir = repoRoot
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("build cerberusd: %v\n%s", err, out.String())
	}
	t.Logf("[real-process] built cerberusd in %s", time.Since(start))
	return binPath
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// freePort asks the OS for an unused TCP port on 127.0.0.1. There is an
// inherent tiny TOCTOU race (the port could theoretically be grabbed by
// something else between Close and the daemon's own Listen), but this is the
// standard Go test pattern and cerberusd's real listen happens milliseconds
// later.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// newRealNode allocates isolated ports and an isolated (but STABLE across
// restart, so identity/config persists like a real crash-and-restart) config
// directory for a node, without starting it yet.
func newRealNode(t *testing.T, repoRoot, binaryPath, id string) *realNode {
	t.Helper()
	base := t.TempDir()
	cfgDir := filepath.Join(base, "cfg-"+id)
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir config dir for %s: %v", id, err)
	}
	return &realNode{
		id:          id,
		binaryPath:  binaryPath,
		repoRoot:    repoRoot,
		configDir:   cfgDir,
		rpcAddr:     fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		gwAddr:      fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		apiAddr:     fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		metricsAddr: fmt.Sprintf("127.0.0.1:%d", freePort(t)),
	}
}

// start execs a fresh real `cerberusd` process for this node. Calling it again
// after kill() (same configDir, same ports) is exactly a crash-and-restart:
// same on-disk issuer key / durable revocation store / eUTXO ledger, but a
// brand-new process, PID, and in-memory state (mesh connections, capRegistry
// catalogue, gossip subscription all start cold).
//
// cerberusd has no --data-dir flag: it derives its persistence directory from
// os.UserConfigDir() (%AppData% on Windows, $XDG_CONFIG_HOME/~/.config
// elsewhere). We isolate each node's persistent state by overriding that
// environment variable for the CHILD PROCESS ONLY (cmd.Env), so two real
// daemons can run side by side on one CI box without corrupting each other's
// bbolt store — this touches no production code, only how the test invokes
// the already-built binary.
func (n *realNode) start(t *testing.T) {
	t.Helper()
	args := []string{
		"-rpc-addr", n.rpcAddr,
		"-gateway-addr", n.gwAddr,
		"-api-addr", n.apiAddr,
		"-metrics-addr", n.metricsAddr,
	}
	cmd := exec.Command(n.binaryPath, args...)
	cmd.Dir = n.repoRoot
	cmd.Env = append(os.Environ(),
		"AppData="+n.configDir,         // Windows: os.UserConfigDir()
		"XDG_CONFIG_HOME="+n.configDir, // Linux
		"HOME="+n.configDir,            // Darwin fallback for os.UserConfigDir()
	)

	logs := &rpSafeBuffer{}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("%s stdout pipe: %v", n.id, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("%s stderr pipe: %v", n.id, err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("%s start: %v", n.id, err)
	}
	go drainRP(n.id, "stdout", stdout, logs)
	go drainRP(n.id, "stderr", stderr, logs)
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	n.mu.Lock()
	n.cmd = cmd
	n.logs = logs
	n.waitErr = waitErr
	n.mu.Unlock()

	t.Logf("[real-process] %s: spawned pid=%d rpc=%s (config=%s)", n.id, cmd.Process.Pid, n.rpcAddr, n.configDir)
}

func drainRP(id, stream string, r io.Reader, logs *rpSafeBuffer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		logs.WriteString(fmt.Sprintf("[%s %s] %s\n", id, stream, scanner.Text()))
	}
}

// kill sends a real, immediate OS-level kill signal to the running process —
// no graceful shutdown, no context cancellation choreography: this is what a
// genuine crash looks like from the outside. It does NOT clear configDir, so a
// subsequent start() is a crash-and-restart with the same on-disk identity.
func (n *realNode) kill(t *testing.T) {
	t.Helper()
	n.mu.Lock()
	cmd := n.cmd
	waitErr := n.waitErr
	n.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		t.Fatalf("%s: kill called before start", n.id)
	}
	killedAt := time.Now()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("%s: os-level kill failed: %v", n.id, err)
	}
	select {
	case <-waitErr:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: process did not exit within 5s of Kill()", n.id)
	}
	t.Logf("[real-process] %s: killed pid=%d in %s (hard OS kill, not graceful shutdown)", n.id, cmd.Process.Pid, time.Since(killedAt))
}

// stop is graceful best-effort teardown for final cleanup (t.Cleanup), not
// part of the fault-injection story.
func (n *realNode) stop() {
	n.mu.Lock()
	cmd := n.cmd
	waitErr := n.waitErr
	n.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	if waitErr != nil {
		select {
		case <-waitErr:
		case <-time.After(2 * time.Second):
		}
	}
}

func (n *realNode) logString() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.logs == nil {
		return ""
	}
	return n.logs.String()
}

// dialRPC connects to the node's control-plane net/rpc server, retrying until
// ctx expires — cerberusd's real startup (mesh + telemetry + 9P + gateway +
// RPC all composing) takes real wall-clock time, so this is a genuine
// readiness poll over a real socket, the RPC analogue of test/e2e's
// waitForDiscovery HTTP poll.
func dialRPC(ctx context.Context, addr string) (*rpc.Client, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if c, err := rpc.Dial("tcp", addr); err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("dial %s: %w", addr, ctx.Err())
		case <-ticker.C:
		}
	}
}

// nodeConfigDir returns the directory the CHILD cerberusd actually derives from
// os.UserConfigDir() given this test's env overrides (AppData=XDG_CONFIG_HOME=
// HOME=configDir). On Windows (%AppData%) and Linux ($XDG_CONFIG_HOME) that IS
// configDir, but on macOS os.UserConfigDir() ignores both and returns
// $HOME/Library/Application Support — so the daemon's token and persistence
// live one level deeper there. The test must read the SAME path the daemon
// wrote, or it waits forever for a token that was written elsewhere (this is
// what timed the test out on the macOS CI runner, and only there).
func nodeConfigDir(configDir string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(configDir, "Library", "Application Support")
	}
	return configDir
}

// operatorToken reads the token cerberusd wrote to THIS node's isolated config
// dir (auth.OperatorTokenPath under the overridden AppData/XDG_CONFIG_HOME/HOME),
// retrying since the daemon writes it slightly after the RPC listener opens.
func operatorToken(ctx context.Context, configDir string) (string, error) {
	path := filepath.Join(nodeConfigDir(configDir), "cerberus", "operator.token")
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if b, err := os.ReadFile(path); err == nil {
			tok := strings.TrimSpace(string(b))
			if tok != "" {
				return tok, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("read operator token at %s: %w", path, ctx.Err())
		case <-ticker.C:
		}
	}
}

// waitMeshConverged polls both nodes' real DaemonRPC.Nodes until each reports
// the other as a connected mesh peer (via real libp2p/mDNS discovery — see
// daemon/mesh/discovery.go's enableMDNS), the real-process equivalent of
// test/e2e's waitForDiscovery.
func waitMeshConverged(ctx context.Context, t *testing.T, a, b *realNode, tokA, tokB string) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if nodesSeesPeer(a.rpcAddr, tokA) && nodesSeesPeer(b.rpcAddr, tokB) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("mesh discovery did not converge within %s\n--- %s logs ---\n%s\n--- %s logs ---\n%s",
				time.Until(deadline), a.id, a.logString(), b.id, b.logString())
		case <-ticker.C:
		}
	}
}

func nodesSeesPeer(addr, token string) bool {
	c, err := rpc.Dial("tcp", addr)
	if err != nil {
		return false
	}
	defer c.Close()
	var resp rpNodesResponse
	if err := c.Call("DaemonRPC.Nodes", &rpNodesRequest{Token: token}, &resp); err != nil {
		return false
	}
	// resp.Nodes always includes self; convergence means we ALSO see a peer.
	return resp.MeshUp && len(resp.Nodes) >= 2
}

// ---- the test ---------------------------------------------------------------

// TestRealProcessKillAndRestartConvergesRevocation is the real-process sibling
// of TestPartitionThenHealConvergesRevocation (partition_test.go). Same
// post-condition family — a capability revocation converges (denies
// mesh-wide) despite one node being unreachable when it happened — but every
// step here crosses a real OS process boundary:
//
//  1. Build the real cerberusd binary once.
//  2. Spawn TWO real cerberusd processes (full daemon: real libp2p/QUIC mesh +
//     mDNS, real bbolt-backed auth.Issuer + RevocationGossip, real net/rpc
//     control surface) with isolated config dirs and distinct ports.
//  3. Wait for REAL mDNS mesh discovery to converge (both daemons' real
//     DaemonRPC.Nodes report the other as a peer) — genuine network activity.
//  4. Mint a capability token on node B via its real RPC (DaemonRPC.CapsMint).
//     cerberusd's token-id counter (auth.Issuer.next) is a fresh in-process
//     sequence that always mints exactly one internal "operator" token at
//     startup before serving any RPC (cmd/cerberusd/main.go), so B's first
//     RPC-driven mint deterministically lands on the SAME id on every boot of
//     that process (e.g. always "2") — record whatever id it actually is.
//  5. KILL node B — a real, immediate os.Process.Kill() (SIGKILL/TerminateProcess),
//     not a graceful shutdown: this is what a genuine mid-operation crash looks
//     like from the outside.
//  6. While B is down, REVOKE that same id on node A via DaemonRPC.CapsRevoke.
//     Revoke takes a bare id string (auth.Issuer.Revoke does not require A to
//     have minted the token), and A's RevocationGossip publishes it onto the
//     real "sys/revocations" pubsub topic on the real mesh — but B is dead, so
//     the live gossip message is genuinely missed (pubsub has no replay/log).
//  7. RESTART B as a fresh process with the SAME config dir (same durable
//     issuer key / bbolt store — a real crash-and-restart preserves identity)
//     and wait for it to REJOIN the mesh via real mDNS discovery again.
//  8. Node A re-announces the revocation (CapsRevoke is idempotent/monotone —
//     auth.Issuer.Revoke marking an already-revoked id is a no-op — mirroring
//     exactly what TestPartitionThenHealConvergesRevocation's Phase 2 does: "a
//     real node re-emits the OR-set union on reconnect").
//  9. Node B mints again post-restart: the per-process id counter restarts
//     fresh and mints the same internal operator token first, so B's first
//     RPC-driven mint after restart deterministically collides on the SAME id
//     that was revoked in step 6. Assert via B's real DaemonRPC.CapsList that
//     this entry now reports Revoked=true: the revocation that happened while
//     B was down converged once B rejoined, observed purely over real RPC
//     against a real process, not via in-process state inspection.
func TestRealProcessKillAndRestartConvergesRevocation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-process chaos test in -short mode (spawns real cerberusd OS processes; takes real wall-clock time for mDNS discovery)")
	}

	testStart := time.Now()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Fatalf("repo root guess %q does not contain go.mod: %v", repoRoot, err)
	}

	binDir := t.TempDir()
	binaryPath := buildCerberusd(t, repoRoot, binDir)

	a := newRealNode(t, repoRoot, binaryPath, "a")
	b := newRealNode(t, repoRoot, binaryPath, "b")
	t.Cleanup(a.stop)
	t.Cleanup(b.stop)

	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()

	spawnStart := time.Now()
	a.start(t)
	b.start(t)

	clientA, err := dialRPC(startCtx, a.rpcAddr)
	if err != nil {
		t.Fatalf("dial a rpc: %v\n--- a logs ---\n%s", err, a.logString())
	}
	defer clientA.Close()
	clientB, err := dialRPC(startCtx, b.rpcAddr)
	if err != nil {
		t.Fatalf("dial b rpc: %v\n--- b logs ---\n%s", err, b.logString())
	}

	tokA, err := operatorToken(startCtx, a.configDir)
	if err != nil {
		t.Fatalf("read a operator token: %v\n--- a logs ---\n%s", err, a.logString())
	}
	tokB, err := operatorToken(startCtx, b.configDir)
	if err != nil {
		t.Fatalf("read b operator token: %v\n--- b logs ---\n%s", err, b.logString())
	}
	t.Logf("[real-process] both daemons ready (rpc dial + operator token) after %s", time.Since(spawnStart))

	// --- Wait for REAL mDNS mesh discovery to converge. ---
	discoveryStart := time.Now()
	convergeCtx, convergeCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer convergeCancel()
	waitMeshConverged(convergeCtx, t, a, b, tokA, tokB)
	t.Logf("[real-process] real mDNS mesh discovery converged after %s", time.Since(discoveryStart))

	// --- Mint a token on B; the first RPC-driven mint of this process
	// deterministically gets whatever id follows the daemon's internal
	// operator-token mint at startup (same counter, same order every boot). ---
	var mintResp rpCapsMintResponse
	if err := clientB.Call("DaemonRPC.CapsMint", &rpCapsMintRequest{
		Token: tokB, Subject: "agent-under-test", Rights: []string{"read"},
	}, &mintResp); err != nil {
		t.Fatalf("caps mint on b: %v", err)
	}
	tokenID := mintResp.ID
	if tokenID == "" {
		t.Fatal("caps mint on b returned an empty token id")
	}
	t.Logf("[real-process] minted token id=%s on b (subject=%s)", tokenID, mintResp.Subject)

	var preRevokeList rpCapsListResponse
	if err := clientB.Call("DaemonRPC.CapsList", &rpCapsListRequest{Token: tokB}, &preRevokeList); err != nil {
		t.Fatalf("caps list on b (pre-revoke): %v", err)
	}
	if revoked, found := findCap(preRevokeList, tokenID); !found || revoked {
		t.Fatalf("expected token %s present and NOT revoked before the fault, found=%v revoked=%v", tokenID, found, revoked)
	}

	// --- FAULT: kill b — a real, immediate OS-level process kill. ---
	b.kill(t)
	clientB.Close() // the connection is dead; do not reuse it.

	// --- While b is down, revoke that id on a. a does not need to have minted
	// the token — auth.Issuer.Revoke operates on a bare id string — and a's
	// RevocationGossip publishes it onto the real "sys/revocations" topic. b is
	// dead, so it genuinely misses this live gossip message (no replay log). ---
	var revokeResp rpCapsRevokeResponse
	if err := clientA.Call("DaemonRPC.CapsRevoke", &rpCapsRevokeRequest{Token: tokA, ID: tokenID}, &revokeResp); err != nil {
		t.Fatalf("caps revoke on a (while b is down): %v", err)
	}
	if !revokeResp.Revoked {
		t.Fatalf("caps revoke on a reported Revoked=false for id %s", tokenID)
	}
	t.Logf("[real-process] revoked id=%s on a while b was down", tokenID)

	// --- RESTART b: fresh process, SAME config dir (same durable issuer key +
	// revocation store), i.e. a real crash-and-restart with preserved identity. ---
	restartStart := time.Now()
	b.start(t)
	restartCtx, restartCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer restartCancel()
	newClientB, err := dialRPC(restartCtx, b.rpcAddr)
	if err != nil {
		t.Fatalf("dial restarted b rpc: %v\n--- b logs ---\n%s", err, b.logString())
	}
	defer newClientB.Close()
	// The operator token file is rewritten fresh on every start (same issuer
	// key, so it still verifies against the same public key), so re-read it.
	newTokB, err := operatorToken(restartCtx, b.configDir)
	if err != nil {
		t.Fatalf("read restarted b operator token: %v\n--- b logs ---\n%s", err, b.logString())
	}
	t.Logf("[real-process] b restarted and rpc-reachable after %s", time.Since(restartStart))

	// --- b rejoins the mesh via real mDNS discovery again. ---
	rejoinStart := time.Now()
	rejoinCtx, rejoinCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer rejoinCancel()
	waitMeshConverged(rejoinCtx, t, a, b, tokA, newTokB)
	t.Logf("[real-process] b rejoined the mesh after %s", time.Since(rejoinStart))

	// --- b mints again post-restart: the per-process id counter restarts fresh
	// and mints the same internal operator token first, so this first RPC-driven
	// mint after restart deterministically collides on the SAME id that was
	// revoked in step 6 — letting us observe convergence through the real
	// CapsList RPC (whose in-memory catalogue was wiped by the restart, so it
	// must be repopulated to be visible at all). ---
	var mintResp2 rpCapsMintResponse
	if err := newClientB.Call("DaemonRPC.CapsMint", &rpCapsMintRequest{
		Token: newTokB, Subject: "agent-under-test-2", Rights: []string{"read"},
	}, &mintResp2); err != nil {
		t.Fatalf("caps mint on restarted b: %v", err)
	}
	if mintResp2.ID != tokenID {
		t.Fatalf("restarted b's first mint got id %q, want the same deterministic id %q the pre-crash mint used (both processes mint one internal operator token before serving RPCs, so the id sequences should match)",
			mintResp2.ID, tokenID)
	}

	// --- a re-announces the revocation now that b is back (idempotent/monotone
	// re-publish — mirrors TestPartitionThenHealConvergesRevocation's heal phase:
	// "a real node re-emits the OR-set union on reconnect"). Gossipsub's mesh
	// overlay needs its own heartbeat (~1s default) to graft the newly
	// (re)connected peer AFTER the underlying libp2p connection is up, which
	// waitMeshConverged does not wait for (it only checks the connection, not
	// pubsub mesh membership) — so this retries the idempotent re-announce a
	// few times rather than firing exactly once into a pubsub mesh that may not
	// have finished forming yet. Retrying the *same* monotone revoke a few times
	// is itself realistic re-announce behaviour, not a test-only workaround. ---
	convergedWithin := 8 * time.Second
	reannounceDeadline := time.Now().Add(convergedWithin)
	var converged bool
	for time.Now().Before(reannounceDeadline) {
		var reRevokeResp rpCapsRevokeResponse
		if err := clientA.Call("DaemonRPC.CapsRevoke", &rpCapsRevokeRequest{Token: tokA, ID: tokenID}, &reRevokeResp); err != nil {
			t.Fatalf("re-revoke on a after b rejoined: %v", err)
		}
		if waitCapRevoked(newClientB, newTokB, tokenID, 500*time.Millisecond) {
			converged = true
			break
		}
	}

	// --- POST-CONDITION: the revocation issued while b was down converged once
	// b rejoined. Observed via CapsList on b (real RPC, real process). ---
	if !converged {
		t.Fatalf("post-restart: b did not converge to Revoked=true for id %s within %s (revocation issued on a while b was down did not propagate after rejoin)",
			tokenID, convergedWithin)
	}
	t.Logf("[real-process] convergence confirmed: b enforces the revocation issued while it was down")

	t.Logf("[real-process] TOTAL wall-clock for kill+restart convergence test: %s (spawn+ready=%s, first mesh converge=%s, restart+rejoin=%s)",
		time.Since(testStart), time.Since(spawnStart), time.Since(discoveryStart), time.Since(restartStart))
}

// findCap looks up a CapEntry by id in a CapsList response.
func findCap(resp rpCapsListResponse, id string) (revoked bool, found bool) {
	for _, c := range resp.Caps {
		if c.ID == id {
			return c.Revoked, true
		}
	}
	return false, false
}

// waitCapRevoked polls DaemonRPC.CapsList on client until the named id reports
// Revoked=true or the timeout elapses.
func waitCapRevoked(client *rpc.Client, token, id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var resp rpCapsListResponse
		if err := client.Call("DaemonRPC.CapsList", &rpCapsListRequest{Token: token}, &resp); err == nil {
			if revoked, found := findCap(resp, id); found && revoked {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	var resp rpCapsListResponse
	if err := client.Call("DaemonRPC.CapsList", &rpCapsListRequest{Token: token}, &resp); err == nil {
		revoked, _ := findCap(resp, id)
		return revoked
	}
	return false
}
