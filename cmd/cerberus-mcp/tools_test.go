package main

// tools_test.go drives the MCP server's real request-handling logic: it
// starts an actual mcp.Server (with our tools registered) and an actual
// mcp.Client, connects them over mcp.NewInMemoryTransports (no OS process, no
// real stdio pipe, no real cerberusd), and asserts on tools/list and
// tools/call exactly as a real client like Claude Code or Cursor would issue
// them. The daemon side is a fakeBackend (fake_backend_test.go), so these
// tests exercise the full JSON-RPC/MCP protocol path without a running
// cerberusd.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestSession builds a connected (client, server-backend) pair for a test.
// The returned ClientSession is closed automatically via t.Cleanup.
func newTestSession(t *testing.T, be *fakeBackend, startToken string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "cerberus-mcp-test", Version: "test"}, nil)
	registerTools(mcpServer, &toolServer{be: be, startToken: startToken})

	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	// Servers must be connected before clients (NewInMemoryTransports doc).
	serverSession, err := mcpServer.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// ---- tools/list --------------------------------------------------------------

func TestToolsList(t *testing.T) {
	cs := newTestSession(t, &fakeBackend{}, "test-token")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	want := []string{
		"cerberus_status",
		"cerberus_run_workload",
		"cerberus_list_nodes",
		"cerberus_list_devices",
		"cerberus_wallet_balance",
		"cerberus_caps_mint",
		"cerberus_caps_attenuate",
		"cerberus_caps_revoke",
		"cerberus_caps_list",
		"cerberus_conflicts_list",
		"cerberus_conflicts_resolve",
		"cerberus_metrics",
	}
	got := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool
	}
	if len(got) != len(want) {
		t.Fatalf("tools/list returned %d tools, want %d: %v", len(got), len(want), toolNames(res.Tools))
	}
	for _, name := range want {
		tool, ok := got[name]
		if !ok {
			t.Fatalf("tools/list missing tool %q; got %v", name, toolNames(res.Tools))
		}
		if tool.Description == "" {
			t.Errorf("tool %q has empty description", name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has nil input schema", name)
		}
	}
}

func toolNames(tools []*mcp.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

// ---- tools/call: cerberus_status ---------------------------------------------

func TestCallStatus_Success(t *testing.T) {
	be := &fakeBackend{statusResp: StatusResponse{
		Version: "0.1.0", State: "Running", Subject: "operator", Profile: "open_mesh",
		Kernel: "wazero", UptimeSec: 42, MeshUp: true, PeerCount: 2, Balance: 1000,
	}}
	cs := newTestSession(t, be, "test-token")
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "cerberus_status"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	if be.lastToken != "test-token" {
		t.Errorf("backend saw token %q, want the server's startup token", be.lastToken)
	}

	var out statusOutput
	decodeStructured(t, res, &out)
	if out.Version != "0.1.0" || out.Subject != "operator" || out.Balance != 1000 || !out.MeshUp || out.PeerCount != 2 {
		t.Fatalf("unexpected status output: %+v", out)
	}
}

func TestCallStatus_NoTokenAnywhere(t *testing.T) {
	// No startup token, no per-call token override: must fail cleanly, not
	// panic, and not silently proceed with an empty token.
	be := &fakeBackend{}
	cs := newTestSession(t, be, "")
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "cerberus_status"})
	if err != nil {
		t.Fatalf("CallTool transport error (want a tool-level error instead): %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true when no token is available, got %+v", res)
	}
	if be.lastToken != "" {
		t.Fatalf("backend should never have been called without a token")
	}
	assertContainsText(t, res, "no capability token")
}

func TestCallStatus_BackendUnreachable(t *testing.T) {
	// Simulates the real failure mode: no daemon running. Must surface as a
	// sensible tool error, not a panic or a raw transport error.
	be := &fakeBackend{statusErr: errNoDaemon}
	cs := newTestSession(t, be, "test-token")
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "cerberus_status"})
	if err != nil {
		t.Fatalf("CallTool transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true when the backend is unreachable, got %+v", res)
	}
	assertContainsText(t, res, "cannot reach cerberusd")
}

func TestCallStatus_PerCallTokenOverride(t *testing.T) {
	be := &fakeBackend{statusResp: StatusResponse{Subject: "narrow-agent"}}
	cs := newTestSession(t, be, "startup-token")
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "cerberus_status",
		Arguments: map[string]any{"token": "override-token"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res.Content)
	}
	if be.lastToken != "override-token" {
		t.Errorf("backend saw token %q, want the per-call override", be.lastToken)
	}
}

// ---- tools/call: cerberus_run_workload ---------------------------------------

func TestCallRunWorkload_Success(t *testing.T) {
	be := &fakeBackend{runResp: RunResponse{
		OK: true, Output: "1337", TaskID: "abc123", Where: "local", CID: "bafy...",
	}}
	cs := newTestSession(t, be, "test-token")
	ctx := context.Background()

	wasmMagic := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "cerberus_run_workload",
		Arguments: map[string]any{
			"component_base64": base64.StdEncoding.EncodeToString(wasmMagic),
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	var out runWorkloadOutput
	decodeStructured(t, res, &out)
	if !out.OK || out.Output != "1337" || out.Where != "local" {
		t.Fatalf("unexpected run output: %+v", out)
	}
}

func TestCallRunWorkload_WorkloadFailure(t *testing.T) {
	be := &fakeBackend{runResp: RunResponse{OK: false, Error: "trap: unreachable"}}
	cs := newTestSession(t, be, "test-token")
	ctx := context.Background()

	wasmMagic := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "cerberus_run_workload",
		Arguments: map[string]any{
			"component_base64": base64.StdEncoding.EncodeToString(wasmMagic),
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true for a failed workload, got %+v", res)
	}
	assertContainsText(t, res, "trap: unreachable")
}

func TestCallRunWorkload_BadBase64(t *testing.T) {
	be := &fakeBackend{}
	cs := newTestSession(t, be, "test-token")
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "cerberus_run_workload",
		Arguments: map[string]any{"component_base64": "!!!not-base64!!!"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true for undecodable base64, got %+v", res)
	}
	if be.lastToken != "" {
		t.Fatalf("backend should not be called when input decoding fails")
	}
}

func TestCallRunWorkload_EmptyComponent(t *testing.T) {
	be := &fakeBackend{}
	cs := newTestSession(t, be, "test-token")
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "cerberus_run_workload",
		Arguments: map[string]any{"component_base64": ""},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true for empty component, got %+v", res)
	}
}

// ---- tools/call: cerberus_list_nodes / list_devices --------------------------

func TestCallListNodes(t *testing.T) {
	be := &fakeBackend{nodesResp: NodesResponse{
		MeshUp: true, SelfPeer: "deadbeef",
		Nodes: []NodeEntry{{PeerID: "deadbeef", Addr: "(this node)", Self: true}, {PeerID: "cafef00d", Addr: "10.0.0.2:9000"}},
	}}
	cs := newTestSession(t, be, "test-token")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "cerberus_list_nodes"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res.Content)
	}
	var out listNodesOutput
	decodeStructured(t, res, &out)
	if !out.MeshUp || len(out.Nodes) != 2 {
		t.Fatalf("unexpected nodes output: %+v", out)
	}
}

func TestCallListDevices(t *testing.T) {
	be := &fakeBackend{devicesResp: DevicesResponse{
		Devices: []DeviceEntry{{Path: "/cer/dev/vram/local/0", Kind: "vram", QuotaBytes: 2 << 30}},
	}}
	cs := newTestSession(t, be, "test-token")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "cerberus_list_devices"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res.Content)
	}
	var out listDevicesOutput
	decodeStructured(t, res, &out)
	if len(out.Devices) != 1 || out.Devices[0].Path != "/cer/dev/vram/local/0" {
		t.Fatalf("unexpected devices output: %+v", out)
	}
}

// ---- tools/call: cerberus_wallet_balance -------------------------------------

func TestCallWalletBalance(t *testing.T) {
	be := &fakeBackend{walletResp: WalletResponse{Owner: "operator", Balance: 999_000, Enabled: true, TotalSupply: 1_000_000}}
	cs := newTestSession(t, be, "test-token")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "cerberus_wallet_balance",
		Arguments: map[string]any{"owner": "operator"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res.Content)
	}
	var out walletBalanceOutput
	decodeStructured(t, res, &out)
	if out.Balance != 999_000 || out.TotalSupply != 1_000_000 {
		t.Fatalf("unexpected wallet output: %+v", out)
	}
}

// ---- tools/call: cerberus_caps_* ---------------------------------------------

func TestCallCapsMint_Success(t *testing.T) {
	be := &fakeBackend{capsMintResp: CapsMintResponse{Token: "tok.sig", ID: "1", Subject: "agent-7"}}
	cs := newTestSession(t, be, "test-token")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "cerberus_caps_mint",
		Arguments: map[string]any{"subject": "agent-7", "rights": []string{"read", "exec"}},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res.Content)
	}
	var out capsMintOutput
	decodeStructured(t, res, &out)
	if out.Subject != "agent-7" || out.ID != "1" {
		t.Fatalf("unexpected mint output: %+v", out)
	}
}

func TestCallCapsMint_MissingSubject(t *testing.T) {
	be := &fakeBackend{}
	cs := newTestSession(t, be, "test-token")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "cerberus_caps_mint"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true for missing subject, got %+v", res)
	}
	if be.lastToken != "" {
		t.Fatalf("backend should not be called with invalid input")
	}
}

func TestCallCapsAttenuate_MissingRights(t *testing.T) {
	be := &fakeBackend{}
	cs := newTestSession(t, be, "test-token")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "cerberus_caps_attenuate",
		Arguments: map[string]any{"parent": "sometoken"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true for missing rights, got %+v", res)
	}
}

func TestCallCapsRevoke(t *testing.T) {
	be := &fakeBackend{capsRevokeResp: CapsRevokeResponse{ID: "1", Revoked: true}}
	cs := newTestSession(t, be, "test-token")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "cerberus_caps_revoke",
		Arguments: map[string]any{"id": "1"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res.Content)
	}
	var out capsRevokeOutput
	decodeStructured(t, res, &out)
	if !out.Revoked {
		t.Fatalf("unexpected revoke output: %+v", out)
	}
}

func TestCallCapsList(t *testing.T) {
	be := &fakeBackend{capsListResp: CapsListResponse{Caps: []CapEntry{
		{ID: "1", Subject: "agent-7", Rights: []string{"read"}, Revoked: false},
	}}}
	cs := newTestSession(t, be, "test-token")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "cerberus_caps_list"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res.Content)
	}
	var out capsListOutput
	decodeStructured(t, res, &out)
	if len(out.Caps) != 1 || out.Caps[0].Subject != "agent-7" {
		t.Fatalf("unexpected caps list output: %+v", out)
	}
}

// ---- tools/call: cerberus_conflicts_* -----------------------------------------

func TestCallConflictsList(t *testing.T) {
	be := &fakeBackend{conflictsListResp: ConflictsListResponse{
		Doc: "abcd", Conflicts: []ConflictEntry{{Subject: "temp", Candidates: []ConflictCandidate{{Actor: "a1", Value: "72"}, {Actor: "a2", Value: "75"}}}},
	}}
	cs := newTestSession(t, be, "test-token")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "cerberus_conflicts_list"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res.Content)
	}
	var out conflictsListOutput
	decodeStructured(t, res, &out)
	if len(out.Conflicts) != 1 || len(out.Conflicts[0].Candidates) != 2 {
		t.Fatalf("unexpected conflicts output: %+v", out)
	}
}

func TestCallConflictsResolve_MissingSubject(t *testing.T) {
	be := &fakeBackend{}
	cs := newTestSession(t, be, "test-token")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "cerberus_conflicts_resolve",
		Arguments: map[string]any{"value": "72"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true for missing subject, got %+v", res)
	}
}

func TestCallConflictsResolve_Success(t *testing.T) {
	be := &fakeBackend{conflictsResolveResp: ConflictsResolveResponse{Resolved: true, Subject: "temp"}}
	cs := newTestSession(t, be, "test-token")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "cerberus_conflicts_resolve",
		Arguments: map[string]any{"subject": "temp", "value": "72"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res.Content)
	}
	var out conflictsResolveOutput
	decodeStructured(t, res, &out)
	if !out.Resolved || out.Subject != "temp" {
		t.Fatalf("unexpected resolve output: %+v", out)
	}
}

// ---- tools/call: cerberus_metrics ---------------------------------------------

func TestCallMetrics(t *testing.T) {
	raw := "# HELP cerberus_peers Connected mesh peers\n# TYPE cerberus_peers gauge\ncerberus_peers 3\ncerberus_wasm_execs_total{status=\"ok\"} 42\n"
	be := &fakeBackend{metricsResp: raw}
	cs := newTestSession(t, be, "test-token")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "cerberus_metrics"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res.Content)
	}
	var out metricsOutput
	decodeStructured(t, res, &out)
	if out.Raw != raw {
		t.Fatalf("raw metrics text mismatch")
	}
	if len(out.Samples) != 2 {
		t.Fatalf("expected 2 parsed samples, got %d: %+v", len(out.Samples), out.Samples)
	}
	foundLabeled := false
	for _, sample := range out.Samples {
		if sample.Name == "cerberus_wasm_execs_total" {
			foundLabeled = true
			if sample.Labels["status"] != "ok" || sample.Value != 42 {
				t.Fatalf("unexpected labeled sample: %+v", sample)
			}
		}
	}
	if !foundLabeled {
		t.Fatalf("did not find labeled sample in %+v", out.Samples)
	}
}

func TestCallMetrics_BackendError(t *testing.T) {
	be := &fakeBackend{metricsErr: errNoDaemon}
	cs := newTestSession(t, be, "test-token")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "cerberus_metrics"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true, got %+v", res)
	}
}

// ---- unknown tool -------------------------------------------------------------

func TestCallUnknownTool(t *testing.T) {
	cs := newTestSession(t, &fakeBackend{}, "test-token")
	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "cerberus_does_not_exist"})
	if err == nil {
		t.Fatalf("expected a protocol-level error calling an unregistered tool")
	}
}

// ---- helpers -------------------------------------------------------------------

// decodeStructured unmarshals a CallToolResult's StructuredContent into out.
// The SDK marshals typed tool outputs there directly.
func decodeStructured(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal structured content into %T: %v", out, err)
	}
}

func assertContainsText(t *testing.T, res *mcp.CallToolResult, substr string) {
	t.Helper()
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok && strings.Contains(tc.Text, substr) {
			return
		}
	}
	t.Fatalf("expected content containing %q, got %+v", substr, res.Content)
}
