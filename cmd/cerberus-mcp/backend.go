package main

// backend.go is the daemon-facing seam. Every MCP tool handler goes through
// the small `backend` interface below instead of calling net/rpc or net/http
// directly, so tests can substitute a fake daemon (see backend_test.go /
// tools_test.go) without spawning a real cerberusd. rpcBackend is the real
// implementation used by main() — it dials the daemon's control-plane RPC
// exactly like cmd/cerberus does, and fetches /metrics exactly like
// cmd/cerberus's `metrics` command does.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/rpc"
	"time"
)

// backend is everything an MCP tool handler needs from a running cerberusd.
// Methods take the operator capability token explicitly (never read it
// themselves) so the MCP server has no ambient authority beyond what the
// caller supplies — matching the repo's ocap principle (CLAUDE.md rule 5).
type backend interface {
	Status(ctx context.Context, req StatusRequest) (StatusResponse, error)
	Run(ctx context.Context, req RunRequest) (RunResponse, error)
	Nodes(ctx context.Context, req NodesRequest) (NodesResponse, error)
	Devices(ctx context.Context, req DevicesRequest) (DevicesResponse, error)
	Wallet(ctx context.Context, req WalletRequest) (WalletResponse, error)
	CapsMint(ctx context.Context, req CapsMintRequest) (CapsMintResponse, error)
	CapsAttenuate(ctx context.Context, req CapsAttenuateRequest) (CapsMintResponse, error)
	CapsRevoke(ctx context.Context, req CapsRevokeRequest) (CapsRevokeResponse, error)
	CapsList(ctx context.Context, req CapsListRequest) (CapsListResponse, error)
	ConflictsList(ctx context.Context, req ConflictsListRequest) (ConflictsListResponse, error)
	ConflictsResolve(ctx context.Context, req ConflictsResolveRequest) (ConflictsResolveResponse, error)
	// Metrics fetches the daemon's raw Prometheus text exposition (the caller
	// presents the token as an HTTP Bearer header, mirroring `cerberus metrics`).
	Metrics(ctx context.Context, token string) (string, error)
}

// rpcBackend talks to a real cerberusd: net/rpc for the control plane
// (rpcAddr), plain HTTP for /metrics (metricsAddr). Both addresses are
// resolved once at startup (see resolveAddrs in main.go) via daemon/discovery,
// falling back to the same hardcoded defaults cmd/cerberus/main.go uses.
type rpcBackend struct {
	rpcAddr     string
	metricsAddr string
	dialTimeout time.Duration
	httpClient  *http.Client
}

func newRPCBackend(rpcAddr, metricsAddr string) *rpcBackend {
	return &rpcBackend{
		rpcAddr:     rpcAddr,
		metricsAddr: metricsAddr,
		dialTimeout: 10 * time.Second,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

// dial opens a fresh net/rpc connection per call. The daemon's RPC server
// (cmd/cerberusd) accepts one connection per client and net/rpc's Client is
// not meant to be shared across unrelated concurrent callers in this
// codebase's usage pattern (cmd/cerberus dials fresh per command too), so we
// mirror that rather than holding a long-lived connection that could go stale
// silently between tool calls.
func (b *rpcBackend) dial(ctx context.Context) (*rpc.Client, error) {
	d := net.Dialer{Timeout: b.dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", b.rpcAddr)
	if err != nil {
		return nil, fmt.Errorf("cannot reach cerberusd at %s: %w (is the daemon running? start it with: cerberusd)", b.rpcAddr, err)
	}
	return rpc.NewClient(conn), nil
}

func call[Req any, Resp any](ctx context.Context, b *rpcBackend, method string, req Req) (Resp, error) {
	var resp Resp
	client, err := b.dial(ctx)
	if err != nil {
		return resp, err
	}
	defer func() { _ = client.Close() }()

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		done <- result{err: client.Call(method, req, &resp)}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return resp, fmt.Errorf("%s: %w", method, r.err)
		}
		return resp, nil
	case <-ctx.Done():
		return resp, ctx.Err()
	}
}

func (b *rpcBackend) Status(ctx context.Context, req StatusRequest) (StatusResponse, error) {
	return call[StatusRequest, StatusResponse](ctx, b, "DaemonRPC.Status", req)
}

func (b *rpcBackend) Run(ctx context.Context, req RunRequest) (RunResponse, error) {
	return call[RunRequest, RunResponse](ctx, b, "DaemonRPC.Run", req)
}

func (b *rpcBackend) Nodes(ctx context.Context, req NodesRequest) (NodesResponse, error) {
	return call[NodesRequest, NodesResponse](ctx, b, "DaemonRPC.Nodes", req)
}

func (b *rpcBackend) Devices(ctx context.Context, req DevicesRequest) (DevicesResponse, error) {
	return call[DevicesRequest, DevicesResponse](ctx, b, "DaemonRPC.Devices", req)
}

func (b *rpcBackend) Wallet(ctx context.Context, req WalletRequest) (WalletResponse, error) {
	return call[WalletRequest, WalletResponse](ctx, b, "DaemonRPC.Wallet", req)
}

func (b *rpcBackend) CapsMint(ctx context.Context, req CapsMintRequest) (CapsMintResponse, error) {
	return call[CapsMintRequest, CapsMintResponse](ctx, b, "DaemonRPC.CapsMint", req)
}

func (b *rpcBackend) CapsAttenuate(ctx context.Context, req CapsAttenuateRequest) (CapsMintResponse, error) {
	return call[CapsAttenuateRequest, CapsMintResponse](ctx, b, "DaemonRPC.CapsAttenuate", req)
}

func (b *rpcBackend) CapsRevoke(ctx context.Context, req CapsRevokeRequest) (CapsRevokeResponse, error) {
	return call[CapsRevokeRequest, CapsRevokeResponse](ctx, b, "DaemonRPC.CapsRevoke", req)
}

func (b *rpcBackend) CapsList(ctx context.Context, req CapsListRequest) (CapsListResponse, error) {
	return call[CapsListRequest, CapsListResponse](ctx, b, "DaemonRPC.CapsList", req)
}

func (b *rpcBackend) ConflictsList(ctx context.Context, req ConflictsListRequest) (ConflictsListResponse, error) {
	return call[ConflictsListRequest, ConflictsListResponse](ctx, b, "DaemonRPC.ConflictsList", req)
}

func (b *rpcBackend) ConflictsResolve(ctx context.Context, req ConflictsResolveRequest) (ConflictsResolveResponse, error) {
	return call[ConflictsResolveRequest, ConflictsResolveResponse](ctx, b, "DaemonRPC.ConflictsResolve", req)
}

func (b *rpcBackend) Metrics(ctx context.Context, token string) (string, error) {
	url := "http://" + b.metricsAddr + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := b.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach metrics endpoint at %s: %w", b.metricsAddr, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metrics: daemon returned %s: %s", res.Status, string(body))
	}
	return string(body), nil
}

var _ backend = (*rpcBackend)(nil)
