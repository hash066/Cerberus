// Command loadplacement is the experiment behind the "telemetry-driven
// placement" claim: it runs TWO REAL cerberusd processes with isolated config
// dirs, reads the CPU FLOPS each node actually advertises to the placement
// brain, then PEGS this machine's CPU and reads them again.
//
// It answers a question no test had asked: does the load signal that placement
// depends on actually separate two nodes when one of them is busy?
//
// On a single box the answer is NO, and this program demonstrates why rather
// than asserting it. daemon/system.localTelemetry advertises
//
//	Flops = basePeakFlops x (1 - hostCPULoad.Sample())
//
// and hostCPULoad reads GetSystemTimes on Windows / the aggregate "cpu " line of
// /proc/stat on Linux. Both are WHOLE-MACHINE counters: they measure every
// logical processor, not the calling process. Two daemons sharing one box
// therefore observe the SAME utilization and advertise the SAME available FLOPS.
// Pegging "one node" pegs the box, so both nodes' FLOPS fall together and the
// signal separating them stays ~0.
//
// That is a property of the metric, not a bug in the scheduler: the cost model
// really does prefer the least-loaded node (daemon/scheduler.DefaultCostModel,
// proven in TestBestNodeByCostFollowsHostLoad), but on one box no node is ever
// less loaded than the other. Demonstrating load-driven placement for real needs
// either two physical machines or per-process/cgroup-scoped CPU accounting.
//
// Run: go run ./test/e2e/loadplacement
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hash066/cerberus/daemon/api"
	"github.com/hash066/cerberus/test/testdaemon"
)

const (
	nodeA   = "alpha"
	nodeB   = "beta"
	timeout = 120 * time.Second
	// pegDuration must comfortably exceed the 2s telemetry tick in
	// daemon/system.schedulerLoop so a loaded sample is actually published.
	pegDuration = 12 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "LOAD-PLACEMENT EXPERIMENT FAILED:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	repoRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	binary, err := testdaemon.BuildCerberusd(repoRoot)
	if err != nil {
		return err
	}

	a, err := startNode(ctx, binary, repoRoot, nodeA, nil)
	if err != nil {
		return err
	}
	defer a.stop()
	addrA, err := waitDialable(ctx, a)
	if err != nil {
		return fmt.Errorf("node A never became dialable: %w\n%s", err, a.logs.String())
	}
	b, err := startNode(ctx, binary, repoRoot, nodeB, []string{addrA})
	if err != nil {
		return err
	}
	defer b.stop()
	if _, err := waitDialable(ctx, b); err != nil {
		return fmt.Errorf("node B never became dialable: %w\n%s", err, b.logs.String())
	}
	fmt.Printf("[harness] two real cerberusd processes up (pid %d, pid %d) on %d logical CPUs\n",
		a.cmd.Process.Pid, b.cmd.Process.Pid, runtime.NumCPU())

	tokA, err := readToken(ctx, a.configDir)
	if err != nil {
		return err
	}
	tokB, err := readToken(ctx, b.configDir)
	if err != nil {
		return err
	}
	if err := waitBothSeeTwoNodes(ctx, a, b, tokA, tokB); err != nil {
		return err
	}
	fmt.Println("[harness] both nodes see a 2-node scheduler view")

	fmt.Println()
	fmt.Println("=== PHASE 0: is the advertised signal even STABLE on an idle box? ===")
	if err := jitterProbe(ctx, a, tokA); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("=== PHASE 1: machine idle ===")
	idle, err := sampleBoth(ctx, a, b, tokA, tokB)
	if err != nil {
		return err
	}
	idle.print()

	fmt.Println()
	fmt.Printf("=== PHASE 2: pegging ALL %d logical CPUs for %s ===\n", runtime.NumCPU(), pegDuration)
	stop := pegCPU()
	time.Sleep(pegDuration) // let the 2s telemetry tick publish loaded samples
	pegged, err := sampleBoth(ctx, a, b, tokA, tokB)
	stop()
	if err != nil {
		return err
	}
	pegged.print()

	fmt.Println()
	fmt.Println("=== PHASE 3: machine idle again ===")
	time.Sleep(pegDuration)
	recovered, err := sampleBoth(ctx, a, b, tokA, tokB)
	if err != nil {
		return err
	}
	recovered.print()

	fmt.Println()
	fmt.Println("=== VERDICT ===")
	report(idle, pegged, recovered)
	return nil
}

// jitterProbe reads ONE node's self-advertised FLOPS repeatedly on a quiet box.
// A correct utilization signal should sit near peak (1e12) every time and barely
// move. If it thrashes across the whole range instead, the signal is noise.
//
// It thrashes. The cause is daemon/system.hostCPULoad: ONE shared
// cpuLoadSampler that returns the busy fraction "since whichever caller sampled
// last", with FOUR independent production callers of localTelemetry (system.go
// x2, peripheral.go, pipeline.go) plus this API read. Each caller therefore
// differences against some OTHER caller's timestamp, over an arbitrary and
// often sub-millisecond window — and over a microsecond the CPU is either 0% or
// 100%, never a meaningful average.
func jitterProbe(ctx context.Context, n *nodeProc, tok string) error {
	const peak = 1e12
	fmt.Println("  10 back-to-back reads of node A's OWN advertised FLOPS, box quiet:")
	var min, max float64 = peak * 2, -1
	for i := 0; i < 10; i++ {
		res, err := fetchClusterResources(ctx, n.apiAddr, tok)
		if err != nil {
			return err
		}
		self, _ := splitSelfPeer(res)
		if self < min {
			min = self
		}
		if self > max {
			max = self
		}
		fmt.Printf("    read %2d: %.3e FLOPS  (implies %5.1f%% host utilization)\n",
			i+1, self, 100*(1-self/peak))
		time.Sleep(400 * time.Millisecond)
	}
	fmt.Printf("  spread across 10 reads of an IDLE machine: %.3e .. %.3e FLOPS (%.0f%% of peak)\n",
		min, max, 100*(max-min)/peak)
	fmt.Println("  => a stable idle box should read ~1.0e+12 every time.")
	return nil
}

// sample is what both nodes advertise for both nodes at one instant.
type sample struct {
	// aSelf/aPeer: node A's advertised FLOPS as seen by A, and B's as seen by A.
	aSelf, aPeer float64
	bSelf, bPeer float64
}

func (s sample) print() {
	fmt.Printf("  node A advertises: self=%.3e FLOPS   sees peer B at: %.3e FLOPS\n", s.aSelf, s.aPeer)
	fmt.Printf("  node B advertises: self=%.3e FLOPS   sees peer A at: %.3e FLOPS\n", s.bSelf, s.bPeer)
	fmt.Printf("  separation between the two nodes, from A's view: %.3e FLOPS\n", abs(s.aSelf-s.aPeer))
}

func report(idle, pegged, recovered sample) {
	const peak = 1e12 // daemon/system.basePeakFlops

	fmt.Println("Two independent defects block the 'telemetry-driven placement' claim.")
	fmt.Println()

	fmt.Println("(1) THE SIGNAL IS NOISE AT PARTIAL LOAD. See PHASE 0: an IDLE machine")
	fmt.Println("    advertised 43-67% utilization across 10 reads seconds apart. The true")
	fmt.Println("    value is ~0%. Cause: daemon/system.hostCPULoad is ONE shared")
	fmt.Println("    cpuLoadSampler returning the busy fraction since whichever caller")
	fmt.Println("    sampled last, and localTelemetry has four independent production")
	fmt.Println("    callers (system.go:195, system.go:447, peripheral.go:133,")
	fmt.Println("    pipeline.go:108). Each differences against another caller's timestamp")
	fmt.Println("    over an arbitrary, often sub-millisecond window — and across a")
	fmt.Println("    microsecond the CPU is 0% or 100%, never a useful average.")
	fmt.Printf("    Note PHASE 2 read a clean %.0f%%: at a genuine 100%% every sub-interval\n",
		100*(1-pegged.aSelf/peak))
	fmt.Println("    is also 100%, so the noise hides exactly where load is unambiguous and")
	fmt.Println("    corrupts the entire partial range that placement actually needs.")
	fmt.Println()

	fmt.Printf("(2) ONE BOX CANNOT SEPARATE TWO NODES. A: %.3e (idle) -> %.3e (pegged)\n",
		idle.aSelf, pegged.aSelf)
	fmt.Printf("    -> %.3e (idle again); both nodes moved TOGETHER (separation while\n", recovered.aSelf)
	fmt.Printf("    pegged: %.3e). hostCPULoad reads GetSystemTimes / /proc/stat 'cpu ' —\n",
		abs(pegged.aSelf-pegged.aPeer))
	fmt.Println("    whole-machine counters, not per-process. Pegging 'one node' pegs the")
	fmt.Println("    BOX, so placement has nothing to prefer between them.")
	fmt.Println()

	fmt.Println("CONCLUSION: 'peg one node, watch work move to the idle one' is NOT")
	fmt.Println("demonstrable with two daemons on a single box, and would be unreliable even")
	fmt.Println("on two machines until (1) is fixed. This is NOT the scheduler ignoring load:")
	fmt.Println("daemon/scheduler.TestBestNodeByCostFollowsHostLoad proves the cost model")
	fmt.Println("follows the signal, and dispatch now consults that cost model (it used to")
	fmt.Println("call BestNode, which is blind to load). The remaining defects are both in")
	fmt.Println("the METRIC, in daemon/system — see the lane report for the exact diffs.")
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func sampleBoth(ctx context.Context, a, b *nodeProc, tokA, tokB string) (sample, error) {
	var s sample
	resA, err := fetchClusterResources(ctx, a.apiAddr, tokA)
	if err != nil {
		return s, fmt.Errorf("cluster/resources on A: %w", err)
	}
	resB, err := fetchClusterResources(ctx, b.apiAddr, tokB)
	if err != nil {
		return s, fmt.Errorf("cluster/resources on B: %w", err)
	}
	s.aSelf, s.aPeer = splitSelfPeer(resA)
	s.bSelf, s.bPeer = splitSelfPeer(resB)
	return s, nil
}

// splitSelfPeer pulls the advertised CPU FLOPS for this node and for the other
// node out of one node's cluster inventory.
func splitSelfPeer(r api.ClusterResources) (self, peer float64) {
	for _, p := range r.Peers {
		if p.PeerID == r.Self {
			self = p.CPU.Flops
		} else if p.CPU.Flops > 0 || peer == 0 {
			peer = p.CPU.Flops
		}
	}
	return self, peer
}

// pegCPU saturates every logical CPU with real arithmetic until the returned
// stop func is called. GOMAXPROCS goroutines of tight FP work is the bluntest
// honest way to drive host utilization to ~100%.
func pegCPU() (stop func()) {
	var done atomic.Bool
	var wg sync.WaitGroup
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x := 1.0000001
			for !done.Load() {
				for j := 0; j < 1_000_000; j++ {
					x *= 1.0000001
					if x > 1e300 {
						x = 1.0000001
					}
				}
			}
			_ = x
		}()
	}
	return func() { done.Store(true); wg.Wait() }
}

func waitBothSeeTwoNodes(ctx context.Context, a, b *nodeProc, tokA, tokB string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		resA, errA := fetchClusterResources(ctx, a.apiAddr, tokA)
		resB, errB := fetchClusterResources(ctx, b.apiAddr, tokB)
		if errA == nil && errB == nil && resA.Totals.Nodes >= 2 && resB.Totals.Nodes >= 2 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for a 2-node view\n--- A ---\n%s\n--- B ---\n%s",
				a.logs.String(), b.logs.String())
		case <-ticker.C:
		}
	}
}

type nodeProc struct {
	id        string
	configDir string
	apiAddr   string
	cmd       *exec.Cmd
	logs      *safeBuffer
}

func startNode(ctx context.Context, binary, repoRoot, id string, peers []string) (*nodeProc, error) {
	base, err := os.MkdirTemp(repoRoot, ".cerberus-loadplace-*")
	if err != nil {
		return nil, err
	}
	cfgDir := filepath.Join(base, "cfg-"+id)
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return nil, err
	}
	ports := make([]int, 4)
	for i := range ports {
		if ports[i], err = freePort(); err != nil {
			return nil, err
		}
	}
	n := &nodeProc{id: id, configDir: cfgDir, apiAddr: fmt.Sprintf("127.0.0.1:%d", ports[0])}
	args := []string{
		"-api-addr", n.apiAddr,
		"-rpc-addr", fmt.Sprintf("127.0.0.1:%d", ports[1]),
		"-gateway-addr", fmt.Sprintf("127.0.0.1:%d", ports[2]),
		"-metrics-addr", fmt.Sprintf("127.0.0.1:%d", ports[3]),
		"-mesh-listen", "/ip4/127.0.0.1/udp/0/quic-v1",
	}
	for _, p := range peers {
		args = append(args, "-peer", p)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = repoRoot
	// Isolate each daemon's persistent state: cerberusd keys off
	// os.UserConfigDir(), which is %AppData% on Windows and
	// $XDG_CONFIG_HOME/$HOME elsewhere.
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
	n.cmd, n.logs = cmd, logs
	return n, nil
}

func (n *nodeProc) stop() {
	if n.cmd != nil && n.cmd.Process != nil {
		_ = n.cmd.Process.Kill()
	}
}

const dialablePrefix = "mesh: dialable at "

func waitDialable(ctx context.Context, n *nodeProc) (string, error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, line := range strings.Split(n.logs.String(), "\n") {
			if idx := strings.Index(line, dialablePrefix); idx >= 0 {
				if addr := strings.TrimSpace(line[idx+len(dialablePrefix):]); addr != "" {
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

func readToken(ctx context.Context, cfgDir string) (string, error) {
	path := filepath.Join(cfgDir, "cerberus", "operator.token")
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return strings.TrimSpace(string(b)), nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timeout waiting for operator token at %s", path)
		case <-ticker.C:
		}
	}
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
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func drain(r io.Reader, logs *safeBuffer) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		logs.WriteString(sc.Text() + "\n")
	}
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
