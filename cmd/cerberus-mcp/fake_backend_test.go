package main

// fake_backend_test.go is a fully in-memory stand-in for a real cerberusd,
// used by tools_test.go and main_test.go so tests never spawn a real daemon or
// pipe real OS stdio. It implements the same `backend` interface rpcBackend
// does, so the tool handlers under test run unmodified against it.

import (
	"context"
	"errors"
)

// fakeBackend is a scriptable backend: each method either returns a canned
// response or, if the matching err* field is set, an error — so tests can
// exercise both the happy path and the "sensible error, not a panic" path the
// task calls for.
type fakeBackend struct {
	statusResp StatusResponse
	statusErr  error

	runResp RunResponse
	runErr  error

	nodesResp NodesResponse
	nodesErr  error

	devicesResp DevicesResponse
	devicesErr  error

	walletResp WalletResponse
	walletErr  error

	capsMintResp CapsMintResponse
	capsMintErr  error

	capsAttenuateResp CapsMintResponse
	capsAttenuateErr  error

	capsRevokeResp CapsRevokeResponse
	capsRevokeErr  error

	capsListResp CapsListResponse
	capsListErr  error

	conflictsListResp ConflictsListResponse
	conflictsListErr  error

	conflictsResolveResp ConflictsResolveResponse
	conflictsResolveErr  error

	metricsResp string
	metricsErr  error

	// lastToken records the token the most recent call was made with, so
	// tests can assert the right token propagated from the tool argument (or
	// the server's startup token) down to the backend call.
	lastToken string
}

var errNoDaemon = errors.New("cannot reach cerberusd at 127.0.0.1:9092: connection refused (is the daemon running? start it with: cerberusd)")

func (f *fakeBackend) Status(_ context.Context, req StatusRequest) (StatusResponse, error) {
	f.lastToken = req.Token
	return f.statusResp, f.statusErr
}

func (f *fakeBackend) Run(_ context.Context, req RunRequest) (RunResponse, error) {
	f.lastToken = req.Token
	return f.runResp, f.runErr
}

func (f *fakeBackend) Nodes(_ context.Context, req NodesRequest) (NodesResponse, error) {
	f.lastToken = req.Token
	return f.nodesResp, f.nodesErr
}

func (f *fakeBackend) Devices(_ context.Context, req DevicesRequest) (DevicesResponse, error) {
	f.lastToken = req.Token
	return f.devicesResp, f.devicesErr
}

func (f *fakeBackend) Wallet(_ context.Context, req WalletRequest) (WalletResponse, error) {
	f.lastToken = req.Token
	return f.walletResp, f.walletErr
}

func (f *fakeBackend) CapsMint(_ context.Context, req CapsMintRequest) (CapsMintResponse, error) {
	f.lastToken = req.Token
	return f.capsMintResp, f.capsMintErr
}

func (f *fakeBackend) CapsAttenuate(_ context.Context, req CapsAttenuateRequest) (CapsMintResponse, error) {
	f.lastToken = req.Token
	return f.capsAttenuateResp, f.capsAttenuateErr
}

func (f *fakeBackend) CapsRevoke(_ context.Context, req CapsRevokeRequest) (CapsRevokeResponse, error) {
	f.lastToken = req.Token
	return f.capsRevokeResp, f.capsRevokeErr
}

func (f *fakeBackend) CapsList(_ context.Context, req CapsListRequest) (CapsListResponse, error) {
	f.lastToken = req.Token
	return f.capsListResp, f.capsListErr
}

func (f *fakeBackend) ConflictsList(_ context.Context, req ConflictsListRequest) (ConflictsListResponse, error) {
	f.lastToken = req.Token
	return f.conflictsListResp, f.conflictsListErr
}

func (f *fakeBackend) ConflictsResolve(_ context.Context, req ConflictsResolveRequest) (ConflictsResolveResponse, error) {
	f.lastToken = req.Token
	return f.conflictsResolveResp, f.conflictsResolveErr
}

func (f *fakeBackend) Metrics(_ context.Context, token string) (string, error) {
	f.lastToken = token
	return f.metricsResp, f.metricsErr
}

var _ backend = (*fakeBackend)(nil)
