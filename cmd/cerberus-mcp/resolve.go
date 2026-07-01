package main

// resolve.go finds a running cerberusd. Resolution order mirrors the "wire
// discoverability into daemon/CLI/tray" workstream (daemon/discovery), which
// every client in this repo is converging on:
//
//  1. daemon/discovery.Read() — the manifest a running cerberusd writes once
//     its subsystems have finished binding, describing the ACTUAL addresses it
//     bound to (not what it hoped to). This is authoritative because it can't
//     drift from reality the way a hardcoded default can.
//  2. $CERBERUS_RPC_ADDR / $CERBERUS_METRICS_ADDR env vars — explicit operator
//     override, same knobs cmd/cerberus/main.go already reads.
//  3. Hardcoded defaults (127.0.0.1:9092 RPC, 127.0.0.1:7779 metrics) — the
//     same fallback cmd/cerberus/main.go uses, so behavior is identical to the
//     CLI when no manifest is present (e.g. an older cerberusd build).
//
// Token resolution goes through daemon/auth.LoadToken(), which already checks
// $CERBERUS_TOKEN then the operator token file — the MCP server does not
// duplicate that logic or embed any token of its own (CLAUDE.md rule 5: no
// ambient authority).

import (
	"os"

	"github.com/hash066/cerberus/daemon/discovery"
)

const (
	defaultRPCAddr     = "127.0.0.1:9092"
	defaultMetricsAddr = "127.0.0.1:7779"
)

// resolvedAddrs is what main() needs to build a backend.
type resolvedAddrs struct {
	RPCAddr     string
	MetricsAddr string
	// Source records where the addresses came from, purely for the server's
	// startup log line (stderr) — never sent to a client, since stdout is the
	// JSON-RPC channel.
	Source string
}

// resolveAddrs implements the discovery/env/default fallback chain described
// above.
func resolveAddrs() resolvedAddrs {
	rpcAddr := envOr("CERBERUS_RPC_ADDR", "")
	metricsAddr := envOr("CERBERUS_METRICS_ADDR", "")
	if rpcAddr != "" || metricsAddr != "" {
		return resolvedAddrs{
			RPCAddr:     orDefault(rpcAddr, defaultRPCAddr),
			MetricsAddr: orDefault(metricsAddr, defaultMetricsAddr),
			Source:      "env",
		}
	}

	if m, err := discovery.Read(); err == nil {
		out := resolvedAddrs{Source: "discovery manifest (" + discovery.Path() + ")"}
		out.RPCAddr = orDefault(m.RPCAddr, defaultRPCAddr)
		out.MetricsAddr = orDefault(m.MetricsAddr, defaultMetricsAddr)
		return out
	}

	return resolvedAddrs{
		RPCAddr:     defaultRPCAddr,
		MetricsAddr: defaultMetricsAddr,
		Source:      "hardcoded defaults (no manifest, no env override)",
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
