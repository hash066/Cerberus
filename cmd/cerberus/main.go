// Command cerberus is the operator CLI for the Cerberus daemon (cerberusd). It
// talks to the daemon's token-gated RPC on 127.0.0.1:9092 and to its metrics
// endpoint on 127.0.0.1:7779. Every command presents the operator capability
// token ($CERBERUS_TOKEN, else the operator token file cerberusd writes), so the
// CLI has no ambient authority — it acts only with the capability it holds.
//
// Commands:
//
//	status                          daemon health, identity, balance
//	run <component.wasm> [--on P]   execute a WASM workload; prints the real result
//	nodes                           list mesh peers (self + connected)
//	devices                         list 9P namespace devices
//	wallet [owner]                  compute-credit balance from the durable ledger
//	caps mint|attenuate|revoke|list capability-token lifecycle
//	components add|list            local named-component registry (name -> CID)
//	fs put|get|ls                  distributed filesystem (erasure-coded, mesh-scattered)
//	gpu <kernel> …                 GPU/CPU compute dispatch (real GPU under -tags ffi)
//	audio loopback|play|monitor …  real-time audio: local data-plane loopback, or cross-node mic/speaker sharing
//	conflicts assert|list|resolve … CRDT belief assertion / conflict inspection / resolution
//	economy challenge …             dispute a pending settlement with a fraud proof
//	metrics                         fetch the local Prometheus /metrics text
//	version                         print the client + contract version
//
// Global: --json prints machine-readable JSON for scripting. Exit code is 0 on
// success, 1 on usage error, 2 on an RPC/daemon error.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/rpc"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/components"
	"github.com/hash066/cerberus/daemon/discovery"
	"github.com/hash066/cerberus/daemon/gpu"
	"github.com/hash066/cerberus/daemon/store"
)

const (
	defaultRPCAddr     = "127.0.0.1:9092"
	defaultMetricsAddr = "127.0.0.1:7779"
)

// resolved bind addresses. Resolution order (highest priority first):
//
//  1. An explicit --rpc / --metrics-addr flag (handled in run(), which
//     overwrites these vars after this initialization).
//  2. $CERBERUS_RPC_ADDR / $CERBERUS_METRICS_ADDR env vars.
//  3. The running daemon's discovery manifest (daemon.json), if present and
//     its PID is still alive — this is what lets the CLI find a daemon that
//     fell back to an ephemeral port after a bind conflict, instead of
//     silently talking to the wrong (or no) port.
//  4. The hardcoded default constants above (today's behavior, unchanged).
//
// So: flag/env > manifest > hardcoded default. A missing or stale manifest
// (no daemon.json, or its PID is dead) is not an error here — it just means
// step 3 contributes nothing and we fall through to step 4, exactly like
// before this change existed.
var (
	rpcAddr     = resolveAddr("CERBERUS_RPC_ADDR", defaultRPCAddr, func(m discovery.Manifest) string { return m.RPCAddr })
	metricsAddr = resolveAddr("CERBERUS_METRICS_ADDR", defaultMetricsAddr, func(m discovery.Manifest) string { return m.MetricsAddr })
)

// resolveAddr implements the env > manifest > default priority described
// above for one address field. pick extracts the relevant field from a
// manifest (empty string if that subsystem never bound).
func resolveAddr(envKey, def string, pick func(discovery.Manifest) string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	if addr, ok := liveManifestAddr(pick); ok {
		return addr
	}
	return def
}

// liveManifestAddr reads the discovery manifest and returns the field pick
// selects, but only when the manifest is readable, names a PID that is still
// alive, AND the requested field is non-empty (a subsystem that failed to
// bind at all leaves its field empty — falling back to the hardcoded default
// is still the right move there, same as a missing manifest).
func liveManifestAddr(pick func(discovery.Manifest) string) (string, bool) {
	m, err := discovery.Read()
	if err != nil {
		return "", false
	}
	if m.PID == 0 || !discovery.IsRunning(m.PID) {
		return "", false
	}
	addr := pick(m)
	if addr == "" {
		return "", false
	}
	return addr, true
}

// exit codes
const (
	exitOK    = 0
	exitUsage = 1
	exitErr   = 2
)

// clientVersion is the CLI build version (tracks the contract for v0.1).
const clientVersion = contract.ContractVersion

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	// Strip global flags from anywhere in the args.
	jsonOut, args := extractFlag(args, "--json")
	if v, rest := extractValueFlag(args, "--rpc"); v != "" {
		rpcAddr, args = v, rest
	}
	if v, rest := extractValueFlag(args, "--metrics-addr"); v != "" {
		metricsAddr, args = v, rest
	}

	if len(args) == 0 {
		usage(os.Stderr)
		return exitUsage
	}

	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "help", "-h", "--help":
		usage(os.Stdout)
		return exitOK
	case "version":
		return cmdVersion(jsonOut)
	case "status":
		return cmdStatus(rest, jsonOut)
	case "run":
		return cmdRun(rest, jsonOut)
	case "pipeline-run":
		return cmdPipelineRun(rest, jsonOut)
	case "nodes":
		return cmdNodes(rest, jsonOut)
	case "devices":
		return cmdDevices(rest, jsonOut)
	case "wallet":
		return cmdWallet(rest, jsonOut)
	case "caps":
		return cmdCaps(rest, jsonOut)
	case "components":
		return cmdComponents(rest, jsonOut)
	case "fs":
		return cmdFS(rest, jsonOut)
	case "gpu":
		return cmdGPU(rest, jsonOut)
	case "audio":
		return cmdAudio(rest, jsonOut)
	case "conflicts":
		return cmdConflicts(rest, jsonOut)
	case "economy":
		return cmdEconomy(rest, jsonOut)
	case "metrics":
		return cmdMetrics(rest, jsonOut)
	case "doctor":
		return cmdDoctor(rest, jsonOut)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage(os.Stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `cerberus — operator CLI for the Cerberus daemon

Usage:
  cerberus [--json] <command> [args]

Commands:
  status                              Daemon health, identity, uptime, balance
  run <component.wasm> [--on <peer>]  Execute a WASM workload; print the real result
  pipeline-run [--model ID] [--backend NAME]  Run a pipeline inference model
  nodes                               List mesh peers (this node + connected)
  devices                             List the 9P namespace devices
  wallet [owner]                      Compute-credit balance from the durable ledger
  caps mint    --subject S [--rights r,r] [--resource path] [--ttl dur]
  caps attenuate --parent TOKEN --rights r,r [--resource path] [--ttl dur]
  caps revoke  <token-id>
  caps list                           List tokens this daemon minted
  components add <name> <path.wasm>   Register a local component under a name
  components list                     List registered components (name, CID, size)
  fs put <local-file> [/cer/fs/name]  Store a file in the distributed FS (erasure-coded, scattered)
  fs get /cer/fs/name [local-file]    Reconstruct a stored file (to a file, or stdout)
  fs cat /cer/fs/name                 Print a stored file to stdout (remote paths over mesh)
  fs ls                               List files stored in /cer/fs (includes remote peers)
  gpu <kernel> <a> [b] [--param N]    Run a compute kernel (vector-add|saxpy|scalar-mul); reports backend (gpu-wgpu with task build:gpu; see docs/gpu.md)
  audio loopback [--freq HZ] [--frames N]  Run a real audio session over the QUIC data plane; report delivery
  audio play --on <peerHexID>          Capture this node's mic and stream it to the PEER's speaker (mesh)
  audio monitor --on <peerHexID>       Play the PEER's mic on this node's speaker (mesh)
  conflicts assert <subject> <value> --agent <name> [--doc <hex>]
                                       Assert an agent belief; concurrent contradictory asserts surface a conflict
  conflicts list [--doc <hex>]        Open CRDT belief conflicts
  conflicts resolve <subject> <value> [--doc <hex>]
  economy challenge <tx-id> --component-cid C --input-cid I --claimed-output-cid O --actual-output-cid A [--challenger P]
                                       Dispute a pending settlement with a fraud proof
  metrics                             Fetch the local Prometheus /metrics text
  doctor                              Diagnose daemon discovery + reachability
  version                             Print client + contract version

Global flags:
  --json    Machine-readable JSON output

Auth:
  Reads $CERBERUS_TOKEN, else the operator token file cerberusd writes
  (`+auth.OperatorTokenPath()+`).

Exit codes: 0 ok, 1 usage error, 2 daemon/RPC error.
`)
}

// ---- command implementations ----------------------------------------------

func cmdVersion(jsonOut bool) int {
	if jsonOut {
		return printJSON(map[string]string{
			"client":   clientVersion,
			"contract": contract.ContractVersion,
		})
	}
	fmt.Printf("cerberus %s (contract %s)\n", clientVersion, contract.ContractVersion)
	return exitOK
}

func cmdStatus(_ []string, jsonOut bool) int {
	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer func() { _ = client.Close() }()

	var resp StatusResponse
	if err := client.Call("DaemonRPC.Status", &StatusRequest{Token: token}, &resp); err != nil {
		return rpcErr("status", err)
	}
	if jsonOut {
		return printJSON(resp)
	}
	fmt.Println("Cerberus Daemon Status")
	fmt.Printf("  Version:  %s\n", resp.Version)
	fmt.Printf("  Profile:  %s\n", resp.Profile)
	fmt.Printf("  Kernel:   %s\n", resp.Kernel)
	fmt.Printf("  State:    %s\n", resp.State)
	fmt.Printf("  Uptime:   %s\n", (time.Duration(resp.UptimeSec) * time.Second).String())
	fmt.Printf("  Auth:     authenticated as %q\n", resp.Subject)
	fmt.Printf("  Mesh:     up=%v peers=%d\n", resp.MeshUp, resp.PeerCount)
	if resp.SelfPeer != "" {
		fmt.Printf("  PeerID:   %s\n", resp.SelfPeer)
	}
	fmt.Printf("  Balance:  %d credits\n", resp.Balance)
	return exitOK
}

func cmdRun(args []string, jsonOut bool) int {
	on, args := extractValueFlag(args, "--on")
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "run: need a WASM component file")
		fmt.Fprintln(os.Stderr, "usage: cerberus run <component.wasm> [--on <peer>]")
		return exitUsage
	}
	path := args[0]
	component, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run: read %s: %v\n", path, err)
		return exitUsage
	}

	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer func() { _ = client.Close() }()

	var resp RunResponse
	req := &RunRequest{Token: token, Component: component, On: on}
	if err := client.Call("DaemonRPC.Run", req, &resp); err != nil {
		return rpcErr("run", err)
	}
	if jsonOut {
		return printJSON(resp)
	}
	where := resp.Where
	if where == "" {
		where = "local"
	}
	fmt.Printf("Task:   %s\n", resp.TaskID)
	fmt.Printf("Where:  %s\n", where)
	if resp.CID != "" {
		fmt.Printf("CID:    %s\n", resp.CID)
	}
	if resp.OK {
		fmt.Printf("Result: %s\n", resp.Output)
		return exitOK
	}
	fmt.Fprintf(os.Stderr, "Result: FAILED: %s\n", resp.Error)
	return exitErr
}

func cmdPipelineRun(args []string, jsonOut bool) int {
	model, args := extractValueFlag(args, "--model")
	backend, args := extractValueFlag(args, "--backend")
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "pipeline-run: unexpected argument %q\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: cerberus pipeline-run [--model ID] [--backend NAME]")
		return exitUsage
	}

	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer func() { _ = client.Close() }()

	var resp PipelineRunResponse
	req := &PipelineRunRequest{Token: token, Model: model, Backend: backend}
	if err := client.Call("DaemonRPC.PipelineRun", req, &resp); err != nil {
		return rpcErr("pipeline-run", err)
	}
	if jsonOut {
		return printJSON(resp)
	}
	if resp.Model != "" {
		fmt.Printf("Model:   %s\n", resp.Model)
	}
	fmt.Printf("Backend: %s\n", resp.Backend)
	for _, st := range resp.Stages {
		where := "local"
		if st.Remote {
			where = "remote"
		}
		fmt.Printf("  layers %d-%d on %s (%s) %s\n", st.LayerLo, st.LayerHi, st.Node, where, st.Duration)
		if !st.OK {
			fmt.Printf("    error: %s\n", st.Error)
		}
	}
	if resp.Content != "" {
		fmt.Printf("Content: %s\n", resp.Content)
	}
	if resp.OK {
		fmt.Printf("Output (%d bytes): %x\n", len(resp.Output), resp.Output)
		return exitOK
	}
	fmt.Fprintf(os.Stderr, "pipeline-run FAILED: %s\n", resp.Error)
	return exitErr
}

func cmdNodes(_ []string, jsonOut bool) int {
	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer func() { _ = client.Close() }()

	var resp NodesResponse
	if err := client.Call("DaemonRPC.Nodes", &NodesRequest{Token: token}, &resp); err != nil {
		return rpcErr("nodes", err)
	}
	if jsonOut {
		return printJSON(resp)
	}
	if !resp.MeshUp {
		fmt.Println("Mesh is not composed on this daemon (no peers).")
		return exitOK
	}
	fmt.Printf("Mesh peers (%d):\n", len(resp.Nodes))
	for _, n := range resp.Nodes {
		tag := ""
		if n.Self {
			tag = "  (self)"
		}
		fmt.Printf("  %s  %s%s\n", n.PeerID, n.Addr, tag)
	}
	return exitOK
}

func cmdDevices(_ []string, jsonOut bool) int {
	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer func() { _ = client.Close() }()

	var resp DevicesResponse
	if err := client.Call("DaemonRPC.Devices", &DevicesRequest{Token: token}, &resp); err != nil {
		return rpcErr("devices", err)
	}
	if jsonOut {
		return printJSON(resp)
	}
	if len(resp.Devices) == 0 {
		fmt.Println("No devices registered in the 9P namespace.")
		return exitOK
	}
	fmt.Printf("9P namespace devices (%d):\n", len(resp.Devices))
	for _, d := range resp.Devices {
		tag := ""
		if d.Pooled && d.Peer != "" {
			tag = fmt.Sprintf("  peer=%s", short(d.Peer))
		}
		name := ""
		if d.Name != "" {
			name = fmt.Sprintf("  name=%q", d.Name)
		}
		fmt.Printf("  %-36s kind=%-6s quota=%s%s%s\n", d.Path, d.Kind, humanBytes(d.QuotaBytes), name, tag)
	}
	return exitOK
}

func cmdWallet(args []string, jsonOut bool) int {
	owner := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		owner = args[0]
	}
	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer func() { _ = client.Close() }()

	var resp WalletResponse
	if err := client.Call("DaemonRPC.Wallet", &WalletRequest{Token: token, Owner: owner}, &resp); err != nil {
		return rpcErr("wallet", err)
	}
	if jsonOut {
		return printJSON(resp)
	}
	fmt.Printf("Wallet: %s\n", resp.Owner)
	fmt.Printf("  Balance:      %d credits\n", resp.Balance)
	fmt.Printf("  Economy:      enabled=%v\n", resp.Enabled)
	fmt.Printf("  Total supply: %d credits (ledger-wide)\n", resp.TotalSupply)
	if len(resp.Transactions) == 0 {
		fmt.Println("  Transactions: none yet (run a workload to record one)")
	} else {
		fmt.Printf("  Transactions (%d most recent; beta logs usage, credits do not move):\n", len(resp.Transactions))
		for _, t := range resp.Transactions {
			when := time.Unix(t.UnixTime, 0).Format("2006-01-02 15:04:05")
			model := t.Model
			if model == "" {
				model = "-"
			}
			fmt.Printf("    #%d  %s  model=%s  %d credit(s)  %s -> %s  [%s]\n",
				t.ID, when, model, t.Amount, t.Consumer, t.Provider, t.State)
		}
	}
	return exitOK
}

func cmdCaps(args []string, jsonOut bool) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "caps: need a subcommand: mint | attenuate | revoke | list")
		return exitUsage
	}
	sub := args[0]
	rest := args[1:]

	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer func() { _ = client.Close() }()

	switch sub {
	case "mint":
		subject, rest := extractValueFlag(rest, "--subject")
		rightsCSV, rest := extractValueFlag(rest, "--rights")
		resource, rest := extractValueFlag(rest, "--resource")
		ttlStr, _ := extractValueFlag(rest, "--ttl")
		if subject == "" {
			fmt.Fprintln(os.Stderr, "caps mint: --subject is required")
			return exitUsage
		}
		ttl, err := parseTTL(ttlStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "caps mint: %v\n", err)
			return exitUsage
		}
		req := &CapsMintRequest{
			Token: token, Subject: subject, Rights: splitCSV(rightsCSV),
			Resource: resource, TTLSecs: ttl,
		}
		var resp CapsMintResponse
		if err := client.Call("DaemonRPC.CapsMint", req, &resp); err != nil {
			return rpcErr("caps mint", err)
		}
		if jsonOut {
			return printJSON(resp)
		}
		fmt.Printf("Minted token for %q (id=%s):\n%s\n", resp.Subject, resp.ID, resp.Token)
		return exitOK

	case "attenuate":
		parent, rest := extractValueFlag(rest, "--parent")
		rightsCSV, rest := extractValueFlag(rest, "--rights")
		resource, rest := extractValueFlag(rest, "--resource")
		ttlStr, _ := extractValueFlag(rest, "--ttl")
		if parent == "" {
			fmt.Fprintln(os.Stderr, "caps attenuate: --parent <token> is required")
			return exitUsage
		}
		ttl, err := parseTTL(ttlStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "caps attenuate: %v\n", err)
			return exitUsage
		}
		req := &CapsAttenuateRequest{
			Token: token, Parent: parent, Rights: splitCSV(rightsCSV),
			Resource: resource, TTLSecs: ttl,
		}
		var resp CapsMintResponse
		if err := client.Call("DaemonRPC.CapsAttenuate", req, &resp); err != nil {
			return rpcErr("caps attenuate", err)
		}
		if jsonOut {
			return printJSON(resp)
		}
		fmt.Printf("Attenuated token (id=%s):\n%s\n", resp.ID, resp.Token)
		return exitOK

	case "revoke":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "caps revoke: need a token id")
			return exitUsage
		}
		req := &CapsRevokeRequest{Token: token, ID: rest[0]}
		var resp CapsRevokeResponse
		if err := client.Call("DaemonRPC.CapsRevoke", req, &resp); err != nil {
			return rpcErr("caps revoke", err)
		}
		if jsonOut {
			return printJSON(resp)
		}
		fmt.Printf("Revoked token id=%s (revoked=%v)\n", resp.ID, resp.Revoked)
		return exitOK

	case "list":
		var resp CapsListResponse
		if err := client.Call("DaemonRPC.CapsList", &CapsListRequest{Token: token}, &resp); err != nil {
			return rpcErr("caps list", err)
		}
		if jsonOut {
			return printJSON(resp)
		}
		if len(resp.Caps) == 0 {
			fmt.Println("No tokens minted by this daemon in this session.")
			return exitOK
		}
		fmt.Printf("Minted tokens (%d):\n", len(resp.Caps))
		for _, c := range resp.Caps {
			state := "active"
			if c.Revoked {
				state = "REVOKED"
			}
			exp := "never"
			if c.Expiry != 0 {
				exp = time.Unix(c.Expiry, 0).Format(time.RFC3339)
			}
			res := c.Resource
			if res == "" {
				res = "*"
			}
			fmt.Printf("  id=%-4s %-8s subject=%-12s rights=%v resource=%s exp=%s\n",
				c.ID, state, c.Subject, c.Rights, res, exp)
		}
		return exitOK

	default:
		fmt.Fprintf(os.Stderr, "caps: unknown subcommand %q (mint | attenuate | revoke | list)\n", sub)
		return exitUsage
	}
}

// ---- components (local named-component registry) --------------------------
//
// This is a purely LOCAL, on-disk convenience — a name -> CID catalogue for
// components you have on this machine, backed by daemon/store (bbolt) in its
// own file so it never contends with a running cerberusd for the daemon's own
// database. It is NOT gated by the daemon's capability-token RPC (there is no
// daemon subsystem here to authorize against) and it is NOT synced across the
// mesh — see daemon/components's package doc for the explicit non-goals.

// componentsDBPath returns the local bbolt file the registry lives in:
// alongside the operator token, in the OS config dir, as its own file (so it
// never lock-contends with cerberusd's cerberus.db).
func componentsDBPath() string {
	return filepath.Join(filepath.Dir(auth.OperatorTokenPath()), "components.db")
}

func openComponentsRegistry() (*components.Registry, *store.Store, int) {
	s, err := store.Open(componentsDBPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "components: open registry: %v\n", err)
		return nil, nil, exitErr
	}
	return components.Open(s), s, exitOK
}

func cmdComponents(args []string, jsonOut bool) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "components: need a subcommand: add | list")
		return exitUsage
	}
	sub := args[0]
	rest := args[1:]

	reg, s, code := openComponentsRegistry()
	if code != exitOK {
		return code
	}
	defer func() { _ = s.Close() }()

	switch sub {
	case "add":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "components add: need <name> <path-to-wasm>")
			fmt.Fprintln(os.Stderr, "usage: cerberus components add <name> <path-to-wasm>")
			return exitUsage
		}
		name, path := rest[0], rest[1]
		entry, err := reg.Add(name, path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "components add: %v\n", err)
			return exitErr
		}
		if jsonOut {
			return printJSON(entry)
		}
		fmt.Printf("Registered %q\n", entry.Name)
		fmt.Printf("  CID:  %s\n", entry.CID)
		fmt.Printf("  Path: %s\n", entry.Path)
		fmt.Printf("  Size: %s\n", humanBytes(uint64(entry.Size)))
		fmt.Println("  (run it with: cerberus run " + entry.Path + ")")
		return exitOK

	case "list":
		list, err := reg.List()
		if err != nil {
			fmt.Fprintf(os.Stderr, "components list: %v\n", err)
			return exitErr
		}
		if jsonOut {
			return printJSON(list)
		}
		if len(list) == 0 {
			fmt.Println("No components registered. Use: cerberus components add <name> <path.wasm>")
			return exitOK
		}
		fmt.Printf("Registered components (%d):\n", len(list))
		for _, e := range list {
			fmt.Printf("  %-20s %-64s %s\n", e.Name, e.CID, humanBytes(uint64(e.Size)))
		}
		return exitOK

	default:
		fmt.Fprintf(os.Stderr, "components: unknown subcommand %q (add | list)\n", sub)
		return exitUsage
	}
}

// ---- audio (real-time session over the data plane) ------------------------

func cmdAudio(args []string, jsonOut bool) int {
	if len(args) == 0 {
		audioUsage()
		return exitUsage
	}
	switch args[0] {
	case "loopback":
		return cmdAudioLoopback(args, jsonOut)
	case "play":
		return cmdAudioSession(args, jsonOut, false)
	case "monitor":
		return cmdAudioSession(args, jsonOut, true)
	default:
		audioUsage()
		return exitUsage
	}
}

func audioUsage() {
	fmt.Fprintln(os.Stderr, "usage: cerberus audio <loopback|play|monitor> [flags]")
	fmt.Fprintln(os.Stderr, "  loopback [--freq HZ] [--frames N]  run a local audio session over the QUIC data plane (no hardware needed)")
	fmt.Fprintln(os.Stderr, "  play    --on <peerHexID>           capture THIS node's mic and stream it to the PEER's speaker")
	fmt.Fprintln(os.Stderr, "  monitor --on <peerHexID>           play the PEER's mic on THIS node's speaker")
}

// cmdAudioSession drives a cross-node mic/speaker session: `audio play --on
// <peer>` (our mic → peer's speaker) or `audio monitor --on <peer>` (peer's mic →
// our speaker). It blocks for the life of the session. Real audio hardware
// (WASAPI on Windows) is required on BOTH nodes to actually hear anything; on a
// machine without a real backend the daemon authorizes the session and then
// reports a clear device-unavailable error rather than faking audio.
func cmdAudioSession(args []string, jsonOut bool, monitor bool) int {
	verb := args[0]
	rest := args[1:]
	on, _ := extractValueFlag(rest, "--on")
	if on == "" {
		fmt.Fprintf(os.Stderr, "usage: cerberus audio %s --on <peerHexID>\n", verb)
		return exitUsage
	}

	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer func() { _ = client.Close() }()

	req := &AudioSessionRequest{Token: token, On: on, Monitor: monitor}
	method := "DaemonRPC.AudioPlay"
	if monitor {
		method = "DaemonRPC.AudioMonitor"
	}
	if !jsonOut {
		if monitor {
			fmt.Printf("Monitoring peer %s microphone on this node's speaker (Ctrl-C to stop)...\n", short(on))
		} else {
			fmt.Printf("Streaming this node's microphone to peer %s speaker (Ctrl-C to stop)...\n", short(on))
		}
	}
	var resp AudioSessionResponse
	if err := client.Call(method, req, &resp); err != nil {
		return rpcErr("audio "+verb, err)
	}
	if jsonOut {
		return printJSON(resp)
	}
	fmt.Printf("Audio %s session with peer %s over %s ended.\n", resp.Direction, short(resp.Peer), resp.Backend)
	return exitOK
}

func cmdAudioLoopback(args []string, jsonOut bool) int {
	rest := args[1:]
	freqStr, rest := extractValueFlag(rest, "--freq")
	framesStr, _ := extractValueFlag(rest, "--frames")
	req := &AudioLoopbackRequest{}
	if freqStr != "" {
		f, err := strconv.ParseFloat(freqStr, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audio: --freq %q: %v\n", freqStr, err)
			return exitUsage
		}
		req.FreqHz = f
	}
	if framesStr != "" {
		n, err := strconv.Atoi(framesStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audio: --frames %q: %v\n", framesStr, err)
			return exitUsage
		}
		req.Frames = n
	}

	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer func() { _ = client.Close() }()

	req.Token = token
	var resp AudioLoopbackResponse
	if err := client.Call("DaemonRPC.AudioLoopback", req, &resp); err != nil {
		return rpcErr("audio", err)
	}
	if jsonOut {
		return printJSON(resp)
	}
	fmt.Printf("Audio session over %s: %d/%d frames delivered (%.0f Hz, %d Hz %dch) in %dms\n",
		resp.Backend, resp.FramesRecv, resp.FramesSent, resp.FreqHz, resp.SampleRate, resp.Channels, resp.DurationMS)
	if resp.FramesRecv == resp.FramesSent {
		fmt.Println("OK — every frame was reconstructed (control-plane grant → data-plane bytes → jitter buffer).")
	}
	return exitOK
}

// ---- gpu (compute dispatch: vector-add / saxpy / scalar-mul) --------------

// parseFloats parses a comma-separated list of f32 (e.g. "1,2,3.5").
func parseFloats(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, 0, len(parts))
	for _, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("bad number %q: %w", p, err)
		}
		out = append(out, float32(f))
	}
	return out, nil
}

// gpuUsage is the detailed help for `cerberus gpu`, including the exact steps a
// Windows + NVIDIA user runs to make the daemon dispatch on the real GPU. Printed
// on `cerberus gpu` with no args (or `--help`).
const gpuUsage = `usage: cerberus gpu <vector-add|saxpy|scalar-mul> <a,b,c> [<d,e,f>] [--param N] [--on <peer>]

Runs an element-wise f32 kernel on the daemon and prints the result plus the
backend that ACTUALLY ran it. Examples:
  cerberus gpu vector-add 1,2,3 4,5,6          # [5 7 9]
  cerberus gpu saxpy 1,2,3 0.5,0.5,0.5 --param 2   # 2*x + y
  cerberus gpu scalar-mul 1,2,3 --param 3      # x * 3
  cerberus gpu vector-add 1,2,3 4,5,6 --on <peerHexID>   # run on peer GPU

Backends (the "backend:" line never lies about what ran):
  cpu-software  real CPU compute; the default build, works with no GPU/toolchain
  gpu-wgpu      the physical GPU via wgpu (e.g. an NVIDIA RTX card)

To get backend: gpu-wgpu on Windows + NVIDIA (one-time), see docs/gpu.md:
  1. install a mingw-w64 GNU C toolchain (zig cc) that cgo can drive, and
     rustup target add x86_64-pc-windows-gnu; rustup component add llvm-tools-preview
  2. build the daemon with the real GPU backend:
       pwsh build/ffi.ps1 -Action build -Gpu     (or: task build:gpu)
  3. run that cerberusd-ffi.exe, then:
       cerberus gpu vector-add 1,2,3 4,5,6   ->   backend: gpu-wgpu
`

func cmdGPU(args []string, jsonOut bool) int {
	paramStr, args := extractValueFlag(args, "--param")
	onPeer, args := extractValueFlag(args, "--on")
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(os.Stderr, gpuUsage)
		return exitUsage
	}
	kernel, ok := gpu.ParseKernel(args[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "gpu: unknown kernel %q (vector-add | saxpy | scalar-mul)\n", args[0])
		return exitUsage
	}
	rest := args[1:]
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "gpu: need at least one input buffer, e.g. cerberus gpu vector-add 1,2,3 10,20,30")
		return exitUsage
	}
	a, err := parseFloats(rest[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "gpu: input a: %v\n", err)
		return exitUsage
	}
	var b []float32
	if len(rest) > 1 {
		if b, err = parseFloats(rest[1]); err != nil {
			fmt.Fprintf(os.Stderr, "gpu: input b: %v\n", err)
			return exitUsage
		}
	}
	var param float32
	if paramStr != "" {
		p, perr := strconv.ParseFloat(paramStr, 32)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "gpu: --param %q: %v\n", paramStr, perr)
			return exitUsage
		}
		param = float32(p)
	}

	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer func() { _ = client.Close() }()

	req := &GpuDispatchRequest{Token: token, Kernel: int(kernel), Param: param, A: a, B: b, On: onPeer}
	var resp GpuDispatchResponse
	if err := client.Call("DaemonRPC.GpuDispatch", req, &resp); err != nil {
		return rpcErr("gpu", err)
	}
	if jsonOut {
		return printJSON(resp)
	}
	fmt.Printf("%s(%s) = %v\n", kernel, formatParam(kernel, param), resp.Output)
	fmt.Printf("backend: %s\n", resp.Backend)
	if resp.Remote {
		fmt.Printf("where: peer %s\n", resp.Where)
	}
	// Honest nudge: if a real GPU did not run, point at the exact way to get one.
	// (Only "cpu-software" means no GPU ran; "gpu-wgpu" or any ffi-fallback string
	// that already names the reason is left as-is.)
	if resp.Backend == "cpu-software" {
		fmt.Println("hint: this is real CPU compute. For the physical GPU (backend: gpu-wgpu), build the daemon with `task build:gpu` — see docs/gpu.md.")
	}
	return exitOK
}

// formatParam renders the kernel's scalar parameter for display (alpha / scalar).
func formatParam(k gpu.Kernel, param float32) string {
	switch k {
	case gpu.Saxpy:
		return fmt.Sprintf("alpha=%g", param)
	case gpu.ScalarMul:
		return fmt.Sprintf("scalar=%g", param)
	default:
		return "a,b"
	}
}

// ---- /cer/fs (distributed filesystem: put / get / ls) ---------------------

func cmdFS(args []string, jsonOut bool) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "fs: need a subcommand: put | get | cat | ls")
		return exitUsage
	}
	sub := args[0]
	rest := args[1:]

	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer func() { _ = client.Close() }()

	switch sub {
	case "put":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: cerberus fs put <local-file> [/cer/fs/<name>]")
			return exitUsage
		}
		local := rest[0]
		data, err := os.ReadFile(local)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fs put: read %s: %v\n", local, err)
			return exitErr
		}
		remote := "/cer/fs/" + filepath.Base(local)
		if len(rest) > 1 {
			remote = rest[1]
		}
		var resp FSPutResponse
		if err := client.Call("DaemonRPC.FSPut", &FSPutRequest{Token: token, Path: remote, Data: data}, &resp); err != nil {
			return rpcErr("fs put", err)
		}
		if jsonOut {
			return printJSON(resp)
		}
		fmt.Printf("Stored %s (%d bytes) — erasure-coded into the distributed filesystem "+
			"(shards placed across mesh peers when present, local-only on a solo node)\n", resp.Path, resp.Bytes)
		return exitOK

	case "get":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: cerberus fs get /cer/fs/<name> [local-file]")
			return exitUsage
		}
		remote := rest[0]
		var resp FSGetResponse
		if err := client.Call("DaemonRPC.FSGet", &FSGetRequest{Token: token, Path: remote}, &resp); err != nil {
			return rpcErr("fs get", err)
		}
		if len(rest) > 1 {
			out := rest[1]
			if err := os.WriteFile(out, resp.Data, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "fs get: write %s: %v\n", out, err)
				return exitErr
			}
			fmt.Printf("Wrote %d bytes to %s\n", len(resp.Data), out)
			return exitOK
		}
		_, _ = os.Stdout.Write(resp.Data) // no local file given: stream to stdout
		return exitOK

	case "cat":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: cerberus fs cat /cer/fs/<name>")
			return exitUsage
		}
		remote := rest[0]
		var resp FSGetResponse
		if err := client.Call("DaemonRPC.FSGet", &FSGetRequest{Token: token, Path: remote}, &resp); err != nil {
			return rpcErr("fs cat", err)
		}
		if jsonOut {
			return printJSON(map[string]any{"path": resp.Path, "bytes": len(resp.Data)})
		}
		_, _ = os.Stdout.Write(resp.Data)
		return exitOK

	case "ls":
		var resp FSListResponse
		if err := client.Call("DaemonRPC.FSList", &FSListRequest{Token: token}, &resp); err != nil {
			return rpcErr("fs ls", err)
		}
		if jsonOut {
			return printJSON(resp)
		}
		if len(resp.Paths) == 0 {
			fmt.Println("No files stored in /cer/fs yet (cerberus fs put <file> to store one).")
			return exitOK
		}
		fmt.Printf("/cer/fs — %d file(s):\n", len(resp.Paths))
		for _, p := range resp.Paths {
			fmt.Printf("  %s\n", p)
		}
		return exitOK

	default:
		fmt.Fprintf(os.Stderr, "fs: unknown subcommand %q (put | get | cat | ls)\n", sub)
		return exitUsage
	}
}

func cmdConflicts(args []string, jsonOut bool) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "conflicts: need a subcommand: assert | list | resolve")
		return exitUsage
	}
	sub := args[0]
	rest := args[1:]

	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer func() { _ = client.Close() }()

	switch sub {
	case "assert":
		doc, rest := extractValueFlag(rest, "--doc")
		agent, rest := extractValueFlag(rest, "--agent")
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "conflicts assert: need <subject> <value>")
			fmt.Fprintln(os.Stderr, "usage: cerberus conflicts assert <subject> <value> --agent <name> [--doc <hex>]")
			return exitUsage
		}
		subject, value := rest[0], rest[1]
		req := &AssertBeliefRequest{Token: token, Doc: doc, Agent: agent, Subject: subject, Value: value}
		var resp AssertBeliefResponse
		if err := client.Call("DaemonRPC.AssertBelief", req, &resp); err != nil {
			return rpcErr("conflicts assert", err)
		}
		if jsonOut {
			return printJSON(resp)
		}
		if resp.Conflict {
			fmt.Printf("Agent %q asserted %q=%q — subject now IN CONFLICT (%d live values: %v)\n",
				resp.Agent, subject, value, len(resp.Values), resp.Values)
		} else {
			fmt.Printf("Agent %q asserted %q=%q (no conflict)\n", resp.Agent, subject, value)
		}
		return exitOK

	case "list":
		doc, _ := extractValueFlag(rest, "--doc")
		var resp ConflictsListResponse
		if err := client.Call("DaemonRPC.ConflictsList", &ConflictsListRequest{Token: token, Doc: doc}, &resp); err != nil {
			return rpcErr("conflicts list", err)
		}
		if jsonOut {
			return printJSON(resp)
		}
		fmt.Printf("Doc %s — open belief conflicts: %d\n", resp.Doc, len(resp.Conflicts))
		for _, c := range resp.Conflicts {
			fmt.Printf("  subject %q:\n", c.Subject)
			for _, cand := range c.Candidates {
				fmt.Printf("    actor=%s value=%q\n", short(cand.Actor), cand.Value)
			}
		}
		return exitOK

	case "resolve":
		doc, rest := extractValueFlag(rest, "--doc")
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "conflicts resolve: need <subject> <value>")
			fmt.Fprintln(os.Stderr, "usage: cerberus conflicts resolve <subject> <value> [--doc <hex>]")
			return exitUsage
		}
		subject, value := rest[0], rest[1]
		req := &ConflictsResolveRequest{Token: token, Doc: doc, Subject: subject, Value: value}
		var resp ConflictsResolveResponse
		if err := client.Call("DaemonRPC.ConflictsResolve", req, &resp); err != nil {
			return rpcErr("conflicts resolve", err)
		}
		if jsonOut {
			return printJSON(resp)
		}
		if resp.Resolved {
			fmt.Printf("Resolved conflict on subject %q -> %q\n", resp.Subject, value)
			return exitOK
		}
		fmt.Printf("No open conflict on subject %q (nothing to resolve)\n", resp.Subject)
		return exitOK

	default:
		fmt.Fprintf(os.Stderr, "conflicts: unknown subcommand %q (assert | list | resolve)\n", sub)
		return exitUsage
	}
}

// ---- economy (optimistic-settlement fraud-proof challenge) ----------------

func cmdEconomy(args []string, jsonOut bool) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "economy: need a subcommand: challenge")
		return exitUsage
	}
	sub := args[0]
	rest := args[1:]

	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer func() { _ = client.Close() }()

	switch sub {
	case "challenge":
		componentCID, rest := extractValueFlag(rest, "--component-cid")
		inputCID, rest := extractValueFlag(rest, "--input-cid")
		claimedCID, rest := extractValueFlag(rest, "--claimed-output-cid")
		actualCID, rest := extractValueFlag(rest, "--actual-output-cid")
		challenger, rest := extractValueFlag(rest, "--challenger")
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "economy challenge: need <tx-id>")
			fmt.Fprintln(os.Stderr, "usage: cerberus economy challenge <tx-id> --component-cid C --input-cid I --claimed-output-cid O --actual-output-cid A [--challenger P]")
			return exitUsage
		}
		tx, err := strconv.ParseUint(rest[0], 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "economy challenge: bad tx id %q: %v\n", rest[0], err)
			return exitUsage
		}
		if componentCID == "" || inputCID == "" || claimedCID == "" || actualCID == "" {
			fmt.Fprintln(os.Stderr, "economy challenge: --component-cid, --input-cid, --claimed-output-cid, and --actual-output-cid are all required")
			return exitUsage
		}
		req := &EconomyChallengeRequest{
			Token:            token,
			Tx:               tx,
			ComponentCID:     componentCID,
			InputCID:         inputCID,
			ClaimedOutputCID: claimedCID,
			ActualOutputCID:  actualCID,
			Challenger:       challenger,
		}
		var resp EconomyChallengeResponse
		if err := client.Call("DaemonRPC.EconomyChallenge", req, &resp); err != nil {
			return rpcErr("economy challenge", err)
		}
		if jsonOut {
			return printJSON(resp)
		}
		fmt.Printf("Challenge on tx %d: SLASHED\n", resp.Tx)
		fmt.Printf("  Refunded to consumer: %d credits\n", resp.Refunded)
		fmt.Printf("  Bond awarded:         %d credits\n", resp.BondAwarded)
		return exitOK

	default:
		fmt.Fprintf(os.Stderr, "economy: unknown subcommand %q (challenge)\n", sub)
		return exitUsage
	}
}

func cmdMetrics(_ []string, jsonOut bool) int {
	token, code := loadToken()
	if code != exitOK {
		return code
	}
	url := "http://" + metricsAddr + "/metrics"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "metrics: %v\n", err)
		return exitErr
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "metrics: cannot reach daemon at %s: %v\n", metricsAddr, err)
		return exitErr
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "metrics: daemon returned %s: %s\n", res.Status, strings.TrimSpace(string(body)))
		return exitErr
	}
	if jsonOut {
		return printJSON(map[string]string{"metrics": string(body)})
	}
	_, _ = os.Stdout.Write(body)
	return exitOK
}

// ---- doctor -----------------------------------------------------------------

// doctorReport is the machine-readable shape `cerberus doctor --json` prints.
// Every field is filled in best-effort: a probe that could not run at all
// (e.g. no manifest to read an address from) is simply omitted/zero rather
// than aborting the rest of the report — the whole point of `doctor` is to
// show what IS and ISN'T reachable, not to require everything to work first.
type doctorReport struct {
	ManifestPath  string `json:"manifest_path"`
	ManifestFound bool   `json:"manifest_found"`
	ManifestError string `json:"manifest_error,omitempty"`

	PID      int  `json:"pid,omitempty"`
	PIDAlive bool `json:"pid_alive"`

	RPCAddr     string `json:"rpc_addr"`
	MetricsAddr string `json:"metrics_addr"`

	Healthz    bool   `json:"healthz"`
	HealthzErr string `json:"healthz_error,omitempty"`
	Readyz     bool   `json:"readyz"`
	ReadyzErr  string `json:"readyz_error,omitempty"`

	RPCReachable bool            `json:"rpc_reachable"`
	RPCErr       string          `json:"rpc_error,omitempty"`
	StatusResp   *StatusResponse `json:"status,omitempty"`
}

// cmdDoctor is "is my setup correctly wired" — the first thing a user or an
// AI coding assistant should run when something seems off. It reads the
// discovery manifest, reports whether the PID it names is alive, and probes
// the metrics /healthz + /readyz endpoints and a trivial RPC (Status) call —
// then prints a clear human-readable (or --json) report of what's reachable.
func cmdDoctor(_ []string, jsonOut bool) int {
	rep := &doctorReport{ManifestPath: discovery.Path()}

	m, merr := discovery.Read()
	if merr != nil {
		rep.ManifestError = merr.Error()
	} else {
		rep.ManifestFound = true
		rep.PID = m.PID
		rep.PIDAlive = m.PID != 0 && discovery.IsRunning(m.PID)
	}

	// Effective addresses: same priority order as the rest of the CLI (flag/env
	// already applied to the package vars by run(); manifest only if live).
	rep.RPCAddr = rpcAddr
	rep.MetricsAddr = metricsAddr

	// Probe /healthz and /readyz on the metrics address (unauthenticated).
	probeClient := &http.Client{Timeout: 5 * time.Second}
	if res, err := probeClient.Get("http://" + rep.MetricsAddr + "/healthz"); err != nil {
		rep.HealthzErr = err.Error()
	} else {
		_ = res.Body.Close()
		rep.Healthz = res.StatusCode == http.StatusOK
		if !rep.Healthz {
			rep.HealthzErr = res.Status
		}
	}
	if res, err := probeClient.Get("http://" + rep.MetricsAddr + "/readyz"); err != nil {
		rep.ReadyzErr = err.Error()
	} else {
		_ = res.Body.Close()
		rep.Readyz = res.StatusCode == http.StatusOK
		if !rep.Readyz {
			rep.ReadyzErr = res.Status
		}
	}

	// Probe the RPC server with a trivial authenticated call (Status).
	if token := auth.LoadToken(); token == "" {
		rep.RPCErr = "no capability token available (set $CERBERUS_TOKEN or start cerberusd)"
	} else if client, err := rpc.Dial("tcp", rep.RPCAddr); err != nil {
		rep.RPCErr = err.Error()
	} else {
		defer func() { _ = client.Close() }()
		var resp StatusResponse
		if err := client.Call("DaemonRPC.Status", &StatusRequest{Token: token}, &resp); err != nil {
			rep.RPCErr = err.Error()
		} else {
			rep.RPCReachable = true
			rep.StatusResp = &resp
		}
	}

	if jsonOut {
		return printJSON(rep)
	}
	printDoctorReport(rep)
	if !rep.Healthz || !rep.RPCReachable {
		return exitErr
	}
	return exitOK
}

func printDoctorReport(rep *doctorReport) {
	fmt.Println("Cerberus Doctor")
	fmt.Println()
	fmt.Println("Discovery manifest:")
	fmt.Printf("  Path:       %s\n", rep.ManifestPath)
	if !rep.ManifestFound {
		fmt.Printf("  Status:     MISSING (%s)\n", rep.ManifestError)
		fmt.Println("              No daemon.json — either cerberusd has never run on this")
		fmt.Println("              machine, or it predates this feature. The CLI is falling back")
		fmt.Println("              to hardcoded default addresses (or --flag/$ENV overrides).")
	} else {
		fmt.Printf("  Status:     found\n")
		fmt.Printf("  PID:        %d (alive=%v)\n", rep.PID, rep.PIDAlive)
		if !rep.PIDAlive {
			fmt.Println("              Manifest is STALE (PID not running) — a prior daemon likely")
			fmt.Println("              crashed without cleaning up. Falling back to hardcoded")
			fmt.Println("              defaults (or --flag/$ENV overrides) until a new daemon starts.")
		}
	}
	fmt.Println()
	fmt.Println("Effective addresses (flag/env > live manifest > hardcoded default):")
	fmt.Printf("  RPC:        %s\n", rep.RPCAddr)
	fmt.Printf("  Metrics:    %s\n", rep.MetricsAddr)
	fmt.Println()
	fmt.Println("Reachability:")
	fmt.Printf("  /healthz:   %s\n", okOrErr(rep.Healthz, rep.HealthzErr))
	fmt.Printf("  /readyz:    %s\n", okOrErr(rep.Readyz, rep.ReadyzErr))
	if rep.RPCReachable {
		fmt.Printf("  RPC Status: ok (version=%s profile=%s mesh_up=%v peers=%d)\n",
			rep.StatusResp.Version, rep.StatusResp.Profile, rep.StatusResp.MeshUp, rep.StatusResp.PeerCount)
	} else {
		fmt.Printf("  RPC Status: FAILED (%s)\n", rep.RPCErr)
	}
	fmt.Println()
	if rep.Healthz && rep.RPCReachable {
		fmt.Println("Overall: cerberusd looks reachable and correctly wired.")
	} else {
		fmt.Println("Overall: one or more daemon surfaces are NOT reachable — see above.")
	}
}

func okOrErr(ok bool, errMsg string) string {
	if ok {
		return "ok"
	}
	if errMsg == "" {
		errMsg = "unreachable"
	}
	return "FAILED (" + errMsg + ")"
}

// ---- shared plumbing -------------------------------------------------------

func loadToken() (string, int) {
	token := auth.LoadToken()
	if token == "" {
		fmt.Fprintf(os.Stderr, "no capability token: set $CERBERUS_TOKEN or start cerberusd (writes %s)\n",
			auth.OperatorTokenPath())
		return "", exitErr
	}
	return token, exitOK
}

func dial() (*rpc.Client, int) {
	client, err := rpc.Dial("tcp", rpcAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot reach cerberusd at %s: %v\n", rpcAddr, err)
		fmt.Fprintln(os.Stderr, "is the daemon running? start it with: cerberusd")
		return nil, exitErr
	}
	return client, exitOK
}

func rpcErr(cmd string, err error) int {
	fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
	return exitErr
}

func printJSON(v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "json: %v\n", err)
		return exitErr
	}
	_, _ = os.Stdout.Write(b)
	fmt.Println()
	return exitOK
}

// extractFlag removes a boolean flag (e.g. --json) from anywhere in args.
func extractFlag(args []string, name string) (bool, []string) {
	out := args[:0:0]
	found := false
	for _, a := range args {
		if a == name {
			found = true
			continue
		}
		out = append(out, a)
	}
	return found, out
}

// extractValueFlag removes "--name value" or "--name=value" from args, returning
// the value (empty if absent) and the remaining args.
func extractValueFlag(args []string, name string) (string, []string) {
	out := make([]string, 0, len(args))
	val := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == name {
			if i+1 < len(args) {
				val = args[i+1]
				i++
			}
			continue
		}
		if strings.HasPrefix(a, name+"=") {
			val = strings.TrimPrefix(a, name+"=")
			continue
		}
		out = append(out, a)
	}
	return val, out
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseTTL parses a duration (e.g. "24h", "30m") or a bare number of seconds,
// returning the TTL in seconds. Empty means no expiry (0).
func parseTTL(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return int64(d.Seconds()), nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	return 0, errors.New("bad --ttl (use e.g. 24h, 30m, or a number of seconds)")
}

func short(s string) string {
	if len(s) > 16 {
		return s[:16] + "…"
	}
	return s
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
