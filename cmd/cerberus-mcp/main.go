// Command cerberus-mcp is an MCP (Model Context Protocol) server that exposes
// a running Cerberus daemon (cerberusd) as a set of tools any MCP-compatible
// AI coding tool (Claude Code, Cursor, etc.) can call directly — the same way
// those tools talk to their own built-in MCP servers.
//
// It is a thin translation layer: every tool wraps the exact control-plane RPC
// the `cerberus` CLI already calls (cmd/cerberusd/rpc.go's DaemonRPC service)
// or the metrics HTTP endpoint, reusing those wire shapes rather than
// inventing a parallel path. It speaks MCP over stdio (the transport Claude
// Code and Cursor both use for locally-installed MCP servers), using the
// official github.com/modelcontextprotocol/go-sdk.
//
// Daemon discovery: resolveAddrs() (resolve.go) reads daemon/discovery's
// connection manifest first (the ACTUAL addresses a running cerberusd bound
// to), then $CERBERUS_RPC_ADDR/$CERBERUS_METRICS_ADDR, then falls back to the
// same hardcoded defaults cmd/cerberus/main.go uses.
//
// Auth: every tool call presents the operator capability token, loaded once at
// startup via daemon/auth.LoadToken() ($CERBERUS_TOKEN, else the operator
// token file cerberusd writes) — or per-call via an optional `token` argument
// on each tool, for testing a narrower/attenuated capability. This process
// embeds no token of its own; it has no ambient authority beyond what that
// token grants (CLAUDE.md rule 5).
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverName/serverVersion identify this MCP server to a connecting client
// during the initialize handshake.
const serverName = "cerberus-mcp"

func main() {
	os.Exit(realMain())
}

func realMain() int {
	addrs := resolveAddrs()

	token := auth.LoadToken()
	if token == "" {
		// Not fatal: a caller can still supply a `token` argument per tool
		// call. But operators should know why every no-token-supplied call
		// will fail, so log it to stderr (stdout is the JSON-RPC channel and
		// must stay clean).
		fmt.Fprintf(os.Stderr,
			"cerberus-mcp: warning: no capability token found at startup "+
				"(set $CERBERUS_TOKEN or start cerberusd, which writes %s); "+
				"tool calls will fail unless a `token` argument is supplied\n",
			auth.OperatorTokenPath())
	}

	fmt.Fprintf(os.Stderr, "cerberus-mcp: contract %s; rpc=%s metrics=%s (%s)\n",
		contract.ContractVersion, addrs.RPCAddr, addrs.MetricsAddr, addrs.Source)

	be := newRPCBackend(addrs.RPCAddr, addrs.MetricsAddr)
	ts := &toolServer{be: be, startToken: token}

	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    serverName,
		Version: contract.ContractVersion,
	}, &mcp.ServerOptions{
		Instructions: "Tools for operating a Cerberus zero-trust distributed hypervisor daemon: " +
			"check status, run WASM workloads across the mesh, inspect mesh peers and 9P devices, " +
			"manage compute-credit wallets and capability tokens, and inspect/resolve CRDT belief conflicts.",
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	registerTools(mcpServer, ts)

	if err := mcpServer.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Printf("cerberus-mcp: server exited: %v", err)
		return 1
	}
	return 0
}
