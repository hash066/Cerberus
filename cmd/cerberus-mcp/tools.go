package main

// tools.go registers every cerberus_* MCP tool on the server. Each handler:
//   1. resolves the operator capability token (the tool's own `token` argument
//      if the caller supplied one — useful for testing a narrower/attenuated
//      capability — else the token this process loaded at startup via
//      auth.LoadToken());
//   2. calls the backend (real RPC/HTTP, or a fake in tests);
//   3. maps a backend error to an MCP tool error (IsError=true + message in
//      Content) rather than a protocol-level error or a panic, so a calling
//      model sees a normal failed-tool-call it can reason about and retry.
//
// No tool ever embeds or hardcodes a token: the server has no ambient
// authority beyond whatever token it was started with or the caller passes
// per-call (CLAUDE.md rule 5).

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// server bundles the backend and the token the process resolved at startup
// (from $CERBERUS_TOKEN or the operator token file via auth.LoadToken()) so
// tool handlers are simple closures over it.
type toolServer struct {
	be         backend
	startToken string
}

// tokenOr returns the per-call token override if non-empty, else the token
// this process loaded at startup.
func (s *toolServer) tokenOr(override string) string {
	if override != "" {
		return override
	}
	return s.startToken
}

// errResult builds a tool-level error result (IsError=true): the MCP spec
// wants tool failures surfaced as content the model can read and react to, not
// as a JSON-RPC protocol error.
func errResult(format string, args ...any) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}, nil
}

func noToken() (*mcp.CallToolResult, error) {
	return errResult("no capability token: set $CERBERUS_TOKEN, start cerberusd (writes the operator token file), " +
		"or pass a `token` argument to this tool")
}

// registerTools wires every cerberus_* tool onto s.
func registerTools(mcpServer *mcp.Server, s *toolServer) {
	registerStatus(mcpServer, s)
	registerRunWorkload(mcpServer, s)
	registerListNodes(mcpServer, s)
	registerListDevices(mcpServer, s)
	registerWalletBalance(mcpServer, s)
	registerCapsMint(mcpServer, s)
	registerCapsAttenuate(mcpServer, s)
	registerCapsRevoke(mcpServer, s)
	registerCapsList(mcpServer, s)
	registerConflictsList(mcpServer, s)
	registerConflictsResolve(mcpServer, s)
	registerMetrics(mcpServer, s)
}

// ---- cerberus_status --------------------------------------------------------

type statusArgs struct {
	Token string `json:"token,omitempty" jsonschema:"operator capability token override; defaults to the token this MCP server was started with"`
}

type statusOutput struct {
	Version   string `json:"version"`
	State     string `json:"state"`
	Subject   string `json:"subject"`
	Profile   string `json:"profile"`
	Kernel    string `json:"kernel"`
	UptimeSec int64  `json:"uptime_sec"`
	MeshUp    bool   `json:"mesh_up"`
	PeerCount int    `json:"peer_count"`
	SelfPeer  string `json:"self_peer,omitempty"`
	Balance   uint64 `json:"balance"`
}

func registerStatus(mcpServer *mcp.Server, s *toolServer) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "cerberus_status",
		Description: "Get the Cerberus daemon's health, identity, mesh, and compute-credit balance snapshot.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args statusArgs) (*mcp.CallToolResult, statusOutput, error) {
		tok := s.tokenOr(args.Token)
		if tok == "" {
			r, err := noToken()
			return r, statusOutput{}, err
		}
		resp, err := s.be.Status(ctx, StatusRequest{Token: tok})
		if err != nil {
			r, _ := errResult("cerberus_status: %v", err)
			return r, statusOutput{}, nil
		}
		return nil, statusOutput(resp), nil
	})
}

// ---- cerberus_run_workload ---------------------------------------------------

type runWorkloadArgs struct {
	Token string `json:"token,omitempty" jsonschema:"operator capability token override"`
	// ComponentBase64 carries the raw WASM component bytes, base64-encoded (MCP
	// tool arguments are JSON, which has no native byte-string type). This is
	// the flagship "do something" tool: it dispatches real wazero execution
	// (or, with On set, a real remote mesh dispatch) and returns the real
	// result — never a canned value (CLAUDE.md maturity-honesty rule).
	ComponentBase64 string `json:"component_base64" jsonschema:"the WASM component to execute, base64-encoded"`
	On              string `json:"on,omitempty" jsonschema:"hex-encoded PeerID of a mesh worker to dispatch to; empty runs locally"`
}

type runWorkloadOutput struct {
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
	TaskID string `json:"task_id"`
	Where  string `json:"where"`
	CID    string `json:"cid,omitempty"`
	Remote bool   `json:"remote"`
}

func registerRunWorkload(mcpServer *mcp.Server, s *toolServer) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "cerberus_run_workload",
		Description: "Execute a WASM component across the Cerberus mesh (locally, or on a named peer via " +
			"`on`) and return the real execution result.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args runWorkloadArgs) (*mcp.CallToolResult, runWorkloadOutput, error) {
		tok := s.tokenOr(args.Token)
		if tok == "" {
			r, err := noToken()
			return r, runWorkloadOutput{}, err
		}
		component, decErr := decodeBase64(args.ComponentBase64)
		if decErr != nil {
			r, _ := errResult("cerberus_run_workload: component_base64: %v", decErr)
			return r, runWorkloadOutput{}, nil
		}
		if len(component) == 0 {
			r, _ := errResult("cerberus_run_workload: component_base64 decoded to zero bytes (need a WASM module)")
			return r, runWorkloadOutput{}, nil
		}
		resp, err := s.be.Run(ctx, RunRequest{Token: tok, Component: component, On: args.On})
		if err != nil {
			r, _ := errResult("cerberus_run_workload: %v", err)
			return r, runWorkloadOutput{}, nil
		}
		out := runWorkloadOutput(resp)
		if !resp.OK {
			// A failed workload execution is a normal (non-protocol) tool
			// error: surface it as IsError so the calling model sees it
			// clearly, but still return the structured fields for detail.
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("workload failed: %s", resp.Error)}},
			}, out, nil
		}
		return nil, out, nil
	})
}

// ---- cerberus_list_nodes -----------------------------------------------------

type listNodesArgs struct {
	Token string `json:"token,omitempty" jsonschema:"operator capability token override"`
}

type nodeEntryOutput struct {
	PeerID string `json:"peer_id"`
	Addr   string `json:"addr"`
	Self   bool   `json:"self"`
}

type listNodesOutput struct {
	SelfPeer string            `json:"self_peer,omitempty"`
	Nodes    []nodeEntryOutput `json:"nodes"`
	MeshUp   bool              `json:"mesh_up"`
}

func registerListNodes(mcpServer *mcp.Server, s *toolServer) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "cerberus_list_nodes",
		Description: "List the Cerberus mesh peers (this node plus connected peers).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listNodesArgs) (*mcp.CallToolResult, listNodesOutput, error) {
		tok := s.tokenOr(args.Token)
		if tok == "" {
			r, err := noToken()
			return r, listNodesOutput{}, err
		}
		resp, err := s.be.Nodes(ctx, NodesRequest{Token: tok})
		if err != nil {
			r, _ := errResult("cerberus_list_nodes: %v", err)
			return r, listNodesOutput{}, nil
		}
		out := listNodesOutput{SelfPeer: resp.SelfPeer, MeshUp: resp.MeshUp}
		for _, n := range resp.Nodes {
			out.Nodes = append(out.Nodes, nodeEntryOutput(n))
		}
		return nil, out, nil
	})
}

// ---- cerberus_list_devices ---------------------------------------------------

type listDevicesArgs struct {
	Token string `json:"token,omitempty" jsonschema:"operator capability token override"`
}

type deviceEntryOutput struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	QuotaBytes uint64 `json:"quota_bytes"`
}

type listDevicesOutput struct {
	Devices []deviceEntryOutput `json:"devices"`
}

func registerListDevices(mcpServer *mcp.Server, s *toolServer) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "cerberus_list_devices",
		Description: "List the devices registered in the Cerberus 9P namespace.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listDevicesArgs) (*mcp.CallToolResult, listDevicesOutput, error) {
		tok := s.tokenOr(args.Token)
		if tok == "" {
			r, err := noToken()
			return r, listDevicesOutput{}, err
		}
		resp, err := s.be.Devices(ctx, DevicesRequest{Token: tok})
		if err != nil {
			r, _ := errResult("cerberus_list_devices: %v", err)
			return r, listDevicesOutput{}, nil
		}
		out := listDevicesOutput{}
		for _, d := range resp.Devices {
			out.Devices = append(out.Devices, deviceEntryOutput(d))
		}
		return nil, out, nil
	})
}

// ---- cerberus_wallet_balance -------------------------------------------------

type walletBalanceArgs struct {
	Token string `json:"token,omitempty" jsonschema:"operator capability token override"`
	Owner string `json:"owner,omitempty" jsonschema:"wallet owner to query; defaults to the caller's subject, then the operator wallet"`
}

type walletBalanceOutput struct {
	Owner       string `json:"owner"`
	Balance     uint64 `json:"balance"`
	Enabled     bool   `json:"enabled"`
	TotalSupply uint64 `json:"total_supply"`
}

func registerWalletBalance(mcpServer *mcp.Server, s *toolServer) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "cerberus_wallet_balance",
		Description: "Get a compute-credit wallet balance from the durable eUTXO ledger.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args walletBalanceArgs) (*mcp.CallToolResult, walletBalanceOutput, error) {
		tok := s.tokenOr(args.Token)
		if tok == "" {
			r, err := noToken()
			return r, walletBalanceOutput{}, err
		}
		resp, err := s.be.Wallet(ctx, WalletRequest{Token: tok, Owner: args.Owner})
		if err != nil {
			r, _ := errResult("cerberus_wallet_balance: %v", err)
			return r, walletBalanceOutput{}, nil
		}
		return nil, walletBalanceOutput(resp), nil
	})
}

// ---- cerberus_caps_mint -------------------------------------------------------

type capsMintArgs struct {
	Token    string   `json:"token,omitempty" jsonschema:"operator capability token override"`
	Subject  string   `json:"subject" jsonschema:"the identity the new token is minted for"`
	Rights   []string `json:"rights,omitempty" jsonschema:"rights to grant, e.g. [read,exec]; defaults to [read]"`
	Resource string   `json:"resource,omitempty" jsonschema:"optional resource path scope; empty means any"`
	TTLSecs  int64    `json:"ttl_secs,omitempty" jsonschema:"time-to-live in seconds; 0 means no expiry"`
}

type capsMintOutput struct {
	Token   string `json:"token"`
	ID      string `json:"id"`
	Subject string `json:"subject"`
}

func registerCapsMint(mcpServer *mcp.Server, s *toolServer) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "cerberus_caps_mint",
		Description: "Mint a new capability token for a subject. Requires the caller's token to carry admin rights.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args capsMintArgs) (*mcp.CallToolResult, capsMintOutput, error) {
		tok := s.tokenOr(args.Token)
		if tok == "" {
			r, err := noToken()
			return r, capsMintOutput{}, err
		}
		if args.Subject == "" {
			r, _ := errResult("cerberus_caps_mint: subject is required")
			return r, capsMintOutput{}, nil
		}
		resp, err := s.be.CapsMint(ctx, CapsMintRequest{
			Token: tok, Subject: args.Subject, Rights: args.Rights,
			Resource: args.Resource, TTLSecs: args.TTLSecs,
		})
		if err != nil {
			r, _ := errResult("cerberus_caps_mint: %v", err)
			return r, capsMintOutput{}, nil
		}
		return nil, capsMintOutput(resp), nil
	})
}

// ---- cerberus_caps_attenuate --------------------------------------------------

type capsAttenuateArgs struct {
	Token    string   `json:"token,omitempty" jsonschema:"operator capability token override"`
	Parent   string   `json:"parent" jsonschema:"the parent token to derive a narrower token from"`
	Rights   []string `json:"rights" jsonschema:"the subset of the parent's rights to keep"`
	Resource string   `json:"resource,omitempty" jsonschema:"optional tighter resource scope"`
	TTLSecs  int64    `json:"ttl_secs,omitempty" jsonschema:"time-to-live in seconds; 0 means no expiry"`
}

func registerCapsAttenuate(mcpServer *mcp.Server, s *toolServer) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "cerberus_caps_attenuate",
		Description: "Derive a strictly narrower capability token from a parent token (delegation).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args capsAttenuateArgs) (*mcp.CallToolResult, capsMintOutput, error) {
		tok := s.tokenOr(args.Token)
		if tok == "" {
			r, err := noToken()
			return r, capsMintOutput{}, err
		}
		if args.Parent == "" {
			r, _ := errResult("cerberus_caps_attenuate: parent is required")
			return r, capsMintOutput{}, nil
		}
		if len(args.Rights) == 0 {
			r, _ := errResult("cerberus_caps_attenuate: at least one right to keep is required")
			return r, capsMintOutput{}, nil
		}
		resp, err := s.be.CapsAttenuate(ctx, CapsAttenuateRequest{
			Token: tok, Parent: args.Parent, Rights: args.Rights,
			Resource: args.Resource, TTLSecs: args.TTLSecs,
		})
		if err != nil {
			r, _ := errResult("cerberus_caps_attenuate: %v", err)
			return r, capsMintOutput{}, nil
		}
		return nil, capsMintOutput(resp), nil
	})
}

// ---- cerberus_caps_revoke -----------------------------------------------------

type capsRevokeArgs struct {
	Token string `json:"token,omitempty" jsonschema:"operator capability token override"`
	ID    string `json:"id" jsonschema:"the token id to revoke"`
}

type capsRevokeOutput struct {
	ID      string `json:"id"`
	Revoked bool   `json:"revoked"`
}

func registerCapsRevoke(mcpServer *mcp.Server, s *toolServer) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "cerberus_caps_revoke",
		Description: "Revoke a capability token by id. Revocation is durable and gossiped mesh-wide.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args capsRevokeArgs) (*mcp.CallToolResult, capsRevokeOutput, error) {
		tok := s.tokenOr(args.Token)
		if tok == "" {
			r, err := noToken()
			return r, capsRevokeOutput{}, err
		}
		if args.ID == "" {
			r, _ := errResult("cerberus_caps_revoke: id is required")
			return r, capsRevokeOutput{}, nil
		}
		resp, err := s.be.CapsRevoke(ctx, CapsRevokeRequest{Token: tok, ID: args.ID})
		if err != nil {
			r, _ := errResult("cerberus_caps_revoke: %v", err)
			return r, capsRevokeOutput{}, nil
		}
		return nil, capsRevokeOutput(resp), nil
	})
}

// ---- cerberus_caps_list -------------------------------------------------------

type capsListArgs struct {
	Token string `json:"token,omitempty" jsonschema:"operator capability token override"`
}

type capEntryOutput struct {
	ID       string   `json:"id"`
	Subject  string   `json:"subject"`
	Rights   []string `json:"rights"`
	Resource string   `json:"resource,omitempty"`
	Expiry   int64    `json:"expiry"`
	Parent   string   `json:"parent,omitempty"`
	Revoked  bool     `json:"revoked"`
}

type capsListOutput struct {
	Caps []capEntryOutput `json:"caps"`
}

func registerCapsList(mcpServer *mcp.Server, s *toolServer) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "cerberus_caps_list",
		Description: "List capability tokens minted by this daemon, with current revocation status.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args capsListArgs) (*mcp.CallToolResult, capsListOutput, error) {
		tok := s.tokenOr(args.Token)
		if tok == "" {
			r, err := noToken()
			return r, capsListOutput{}, err
		}
		resp, err := s.be.CapsList(ctx, CapsListRequest{Token: tok})
		if err != nil {
			r, _ := errResult("cerberus_caps_list: %v", err)
			return r, capsListOutput{}, nil
		}
		out := capsListOutput{}
		for _, c := range resp.Caps {
			out.Caps = append(out.Caps, capEntryOutput(c))
		}
		return nil, out, nil
	})
}

// ---- cerberus_conflicts_list --------------------------------------------------

type conflictsListArgs struct {
	Token string `json:"token,omitempty" jsonschema:"operator capability token override"`
	Doc   string `json:"doc,omitempty" jsonschema:"hex-encoded CRDT doc id; empty means the daemon's default belief doc"`
}

type conflictCandidateOutput struct {
	Actor string `json:"actor"`
	Value string `json:"value"`
}

type conflictEntryOutput struct {
	Subject    string                    `json:"subject"`
	Candidates []conflictCandidateOutput `json:"candidates"`
}

type conflictsListOutput struct {
	Doc       string                `json:"doc"`
	Conflicts []conflictEntryOutput `json:"conflicts"`
}

func registerConflictsList(mcpServer *mcp.Server, s *toolServer) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "cerberus_conflicts_list",
		Description: "List open CRDT belief conflicts for a doc (concurrent, causally-incomparable writes to the same subject).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args conflictsListArgs) (*mcp.CallToolResult, conflictsListOutput, error) {
		tok := s.tokenOr(args.Token)
		if tok == "" {
			r, err := noToken()
			return r, conflictsListOutput{}, err
		}
		resp, err := s.be.ConflictsList(ctx, ConflictsListRequest{Token: tok, Doc: args.Doc})
		if err != nil {
			r, _ := errResult("cerberus_conflicts_list: %v", err)
			return r, conflictsListOutput{}, nil
		}
		out := conflictsListOutput{Doc: resp.Doc}
		for _, c := range resp.Conflicts {
			entry := conflictEntryOutput{Subject: c.Subject}
			for _, cand := range c.Candidates {
				entry.Candidates = append(entry.Candidates, conflictCandidateOutput(cand))
			}
			out.Conflicts = append(out.Conflicts, entry)
		}
		return nil, out, nil
	})
}

// ---- cerberus_conflicts_resolve -----------------------------------------------

type conflictsResolveArgs struct {
	Token   string `json:"token,omitempty" jsonschema:"operator capability token override"`
	Doc     string `json:"doc,omitempty" jsonschema:"hex-encoded CRDT doc id; empty means the daemon's default belief doc"`
	Subject string `json:"subject" jsonschema:"the conflicted subject to resolve"`
	Value   string `json:"value" jsonschema:"the value to record as the causally-dominating resolution"`
}

type conflictsResolveOutput struct {
	Resolved bool   `json:"resolved"`
	Subject  string `json:"subject"`
}

func registerConflictsResolve(mcpServer *mcp.Server, s *toolServer) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "cerberus_conflicts_resolve",
		Description: "Record a human/operator decision that resolves an open CRDT belief conflict on a subject.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args conflictsResolveArgs) (*mcp.CallToolResult, conflictsResolveOutput, error) {
		tok := s.tokenOr(args.Token)
		if tok == "" {
			r, err := noToken()
			return r, conflictsResolveOutput{}, err
		}
		if args.Subject == "" {
			r, _ := errResult("cerberus_conflicts_resolve: subject is required")
			return r, conflictsResolveOutput{}, nil
		}
		resp, err := s.be.ConflictsResolve(ctx, ConflictsResolveRequest{
			Token: tok, Doc: args.Doc, Subject: args.Subject, Value: args.Value,
		})
		if err != nil {
			r, _ := errResult("cerberus_conflicts_resolve: %v", err)
			return r, conflictsResolveOutput{}, nil
		}
		return nil, conflictsResolveOutput(resp), nil
	})
}

// ---- cerberus_metrics ---------------------------------------------------------

type metricsArgs struct {
	Token string `json:"token,omitempty" jsonschema:"operator capability token override"`
}

type metricsOutput struct {
	Samples []MetricSample `json:"samples"`
	Raw     string         `json:"raw"`
}

func registerMetrics(mcpServer *mcp.Server, s *toolServer) {
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "cerberus_metrics",
		Description: "Fetch the daemon's Prometheus metrics as structured samples (name/labels/value), plus the raw exposition text.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args metricsArgs) (*mcp.CallToolResult, metricsOutput, error) {
		tok := s.tokenOr(args.Token)
		if tok == "" {
			r, err := noToken()
			return r, metricsOutput{}, err
		}
		raw, err := s.be.Metrics(ctx, tok)
		if err != nil {
			r, _ := errResult("cerberus_metrics: %v", err)
			return r, metricsOutput{}, nil
		}
		return nil, metricsOutput{Samples: parseMetricsText(raw), Raw: raw}, nil
	})
}
