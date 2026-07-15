// session.go is the GPU-over-DATA-PLANE path (ARCHITECTURE §4.1, vertical 04 §3):
// the control plane (9P namespace) grants a data-plane session by minting a
// transfer when a holder opens /cer/dev/gpu/<node>/0/ctl; the ACTUAL kernel work
// then rides that QUIC data-plane session, never the 9P wire. This file is the
// codec + endpoints for that session:
//
//   - Serve(req) decodes an f32 kernel request, runs it on THIS node's best
//     backend via Dispatch (wgpu under -tags ffi --features gpu, else the real
//     software backend), and encodes the result. It is wired as the
//     dataplane.Responder for a /cer/dev/gpu ctl grant in daemon/system.Compose,
//     so a peer that walked+opened the device (capability-checked by 9P) and dials
//     the granted session gets real GPU work back.
//   - RequestSession is the requester side: encode a kernel, send it over the
//     granted session with dataplane.Client.Request, decode the result + the
//     backend name that actually ran (maturity honesty: a peer that fell back to
//     software says so).
//
// This is distinct from remote.go's mesh path (daemon/mesh/gpu.go): that dispatches
// a kernel over a point-to-point libp2p RPC keyed by a signed capability; THIS
// dispatches over the 9P-granted data-plane session, the "mount a peer's GPU as a
// device" model. Both run the same Dispatch backend and report the same honest
// backend string.
package gpu

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hash066/cerberus/daemon/dataplane"
)

// sessionRequest is the on-wire kernel request carried over the data-plane
// session. KernelID mirrors the daemon/gpu + core/cabi ABI (0=VectorAdd,
// 1=Saxpy(alpha=Param), 2=ScalarMul(scalar=Param); B is ignored for ScalarMul).
// JSON keeps it inspectable and dependency-free, matching daemon/mesh/gpu.go.
type sessionRequest struct {
	KernelID int       `json:"kernel_id"`
	Param    float32   `json:"param"`
	A        []float32 `json:"a"`
	B        []float32 `json:"b,omitempty"`
}

// sessionResponse is the on-wire result. OK=false carries a human error (a bad
// kernel, mismatched inputs) so the transfer itself still succeeds while the
// application-level failure is surfaced honestly — never a fabricated result.
type sessionResponse struct {
	OK      bool      `json:"ok"`
	Output  []float32 `json:"output,omitempty"`
	Backend string    `json:"backend,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// EncodeRequest marshals a kernel dispatch for the data-plane session.
func EncodeRequest(k Kernel, param float32, a, b []float32) ([]byte, error) {
	return json.Marshal(sessionRequest{KernelID: int(k), Param: param, A: a, B: b})
}

// Serve is the dataplane.Responder body for a /cer/dev/gpu ctl grant: it decodes
// the request, runs the kernel on this node's best backend, and encodes the
// result. It returns a non-error response even for an invalid kernel (OK=false
// with the reason) so the requester learns exactly what failed; it returns a Go
// error only if the request bytes cannot be decoded at all (a malformed session)
// or the response cannot be marshaled.
func Serve(req []byte) ([]byte, error) {
	var r sessionRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return nil, fmt.Errorf("gpu: decode session request: %w", err)
	}
	out, backend, derr := Dispatch(Kernel(r.KernelID), r.Param, r.A, r.B)
	if derr != nil {
		return json.Marshal(sessionResponse{OK: false, Error: derr.Error()})
	}
	return json.Marshal(sessionResponse{OK: true, Output: out, Backend: backend})
}

// RequestSession dispatches a kernel over a data-plane session the holder already
// obtained by opening a /cer/dev/gpu ctl (ep is that granted endpoint). It returns
// the result buffer and the backend that ACTUALLY ran it on the serving node. The
// request is bounded by ep.Quota; an over-quota request or a worker-side failure
// returns an error rather than a fabricated result.
func RequestSession(ctx context.Context, client *dataplane.Client, ep dataplane.Endpoint, k Kernel, param float32, a, b []float32) (out []float32, backend string, err error) {
	if err := validate(k, a, b); err != nil {
		return nil, "", err
	}
	reqBytes, err := EncodeRequest(k, param, a, b)
	if err != nil {
		return nil, "", err
	}
	respBytes, err := client.Request(ctx, ep, reqBytes)
	if err != nil {
		return nil, "", err
	}
	var resp sessionResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, "", fmt.Errorf("gpu: decode session response: %w", err)
	}
	if !resp.OK {
		return nil, "", fmt.Errorf("gpu: session dispatch failed: %s", resp.Error)
	}
	return resp.Output, resp.Backend, nil
}
