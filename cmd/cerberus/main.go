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
//	conflicts list|resolve …        CRDT belief-conflict inspection / resolution
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
	"strconv"
	"strings"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
)

const (
	defaultRPCAddr     = "127.0.0.1:9092"
	defaultMetricsAddr = "127.0.0.1:7779"
)

// resolved bind addresses (overridable via --rpc/--metrics flags or the
// $CERBERUS_RPC_ADDR / $CERBERUS_METRICS_ADDR env vars, so the CLI can target a
// non-default daemon on the same box).
var (
	rpcAddr     = envOr("CERBERUS_RPC_ADDR", defaultRPCAddr)
	metricsAddr = envOr("CERBERUS_METRICS_ADDR", defaultMetricsAddr)
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
	case "nodes":
		return cmdNodes(rest, jsonOut)
	case "devices":
		return cmdDevices(rest, jsonOut)
	case "wallet":
		return cmdWallet(rest, jsonOut)
	case "caps":
		return cmdCaps(rest, jsonOut)
	case "conflicts":
		return cmdConflicts(rest, jsonOut)
	case "metrics":
		return cmdMetrics(rest, jsonOut)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage(os.Stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `cerberus — operator CLI for the Cerberus daemon

Usage:
  cerberus [--json] <command> [args]

Commands:
  status                              Daemon health, identity, uptime, balance
  run <component.wasm> [--on <peer>]  Execute a WASM workload; print the real result
  nodes                               List mesh peers (this node + connected)
  devices                             List the 9P namespace devices
  wallet [owner]                      Compute-credit balance from the durable ledger
  caps mint    --subject S [--rights r,r] [--resource path] [--ttl dur]
  caps attenuate --parent TOKEN --rights r,r [--resource path] [--ttl dur]
  caps revoke  <token-id>
  caps list                           List tokens this daemon minted
  conflicts list [--doc <hex>]        Open CRDT belief conflicts
  conflicts resolve <subject> <value> [--doc <hex>]
  metrics                             Fetch the local Prometheus /metrics text
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
	defer client.Close()

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
	defer client.Close()

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

func cmdNodes(_ []string, jsonOut bool) int {
	token, code := loadToken()
	if code != exitOK {
		return code
	}
	client, code := dial()
	if code != exitOK {
		return code
	}
	defer client.Close()

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
	defer client.Close()

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
		fmt.Printf("  %-28s kind=%-6s quota=%s\n", d.Path, d.Kind, humanBytes(d.QuotaBytes))
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
	defer client.Close()

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
	defer client.Close()

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

func cmdConflicts(args []string, jsonOut bool) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "conflicts: need a subcommand: list | resolve")
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
	defer client.Close()

	switch sub {
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
		fmt.Fprintf(os.Stderr, "conflicts: unknown subcommand %q (list | resolve)\n", sub)
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
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "metrics: daemon returned %s: %s\n", res.Status, strings.TrimSpace(string(body)))
		return exitErr
	}
	if jsonOut {
		return printJSON(map[string]string{"metrics": string(body)})
	}
	os.Stdout.Write(body)
	return exitOK
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
	os.Stdout.Write(b)
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
