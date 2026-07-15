package mesh

// activation.go is the mesh control-plane hook that lets a pipeline orchestrator
// ask a remote node to register a data-plane grant for an inbound activation
// tensor. Bulk bytes ride the QUIC data plane (RegisterGrant + Client.Send);
// this RPC only mints the grant and returns the Endpoint descriptor.

import (
	"context"
	"encoding/json"
	"fmt"
	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/dataplane"
	"github.com/libp2p/go-libp2p/core/network"
)

const activationProto = "/cerberus/pipeline/activation/1.0.0"

// ActivationGrantHandler registers a one-shot data-plane sink for an inbound
// activation on the worker and returns the Endpoint the sender should dial.
type ActivationGrantHandler func(quota contract.Quota) (dataplane.Endpoint, error)

type activationGrantRequest struct {
	Bytes uint64 `json:"bytes"`
}

type activationGrantResponse struct {
	Endpoint dataplane.Endpoint `json:"endpoint"`
}

// ServeActivationGrant registers the worker-side handler that prepares an inbound
// activation transfer on the local data plane.
func (f *Fabric) ServeActivationGrant(h ActivationGrantHandler) {
	f.host.SetStreamHandler(activationProto, func(s network.Stream) {
		ss := newStreamSession(s)
		defer func() { _ = ss.Close() }()

		raw, err := ss.Recv()
		if err != nil {
			_ = s.Reset()
			return
		}
		var req activationGrantRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			_ = ss.Send(mustMarshalActivationResp(activationGrantResponse{}))
			return
		}
		if req.Bytes == 0 {
			req.Bytes = 4096
		}
		ep, err := h(contract.Quota{Bytes: req.Bytes})
		if err != nil {
			_ = ss.Send(mustMarshalActivationResp(activationGrantResponse{}))
			return
		}
		_ = ss.Send(mustMarshalActivationResp(activationGrantResponse{Endpoint: ep}))
	})
}

// RequestActivationGrant asks a remote peer to register a data-plane grant sized
// for an activation of n bytes and returns the Endpoint to send to.
func (f *Fabric) RequestActivationGrant(ctx context.Context, worker contract.PeerID, n uint64) (dataplane.Endpoint, error) {
	pid, err := toLibp2pID(worker)
	if err != nil {
		return dataplane.Endpoint{}, err
	}
	s, err := f.host.NewStream(ctx, pid, activationProto)
	if err != nil {
		return dataplane.Endpoint{}, contract.Errf(contract.ErrPartitioned, err.Error())
	}
	ss := newStreamSession(s)
	defer func() { _ = ss.Close() }()

	body, err := json.Marshal(activationGrantRequest{Bytes: n})
	if err != nil {
		return dataplane.Endpoint{}, err
	}
	if err := ss.Send(body); err != nil {
		return dataplane.Endpoint{}, contract.Errf(contract.ErrPartitioned, err.Error())
	}
	raw, err := ss.Recv()
	if err != nil {
		return dataplane.Endpoint{}, err
	}
	var resp activationGrantResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return dataplane.Endpoint{}, fmt.Errorf("mesh: decode activation grant: %w", err)
	}
	if resp.Endpoint.Addr == "" {
		return dataplane.Endpoint{}, fmt.Errorf("mesh: peer refused activation grant")
	}
	return resp.Endpoint, nil
}

func mustMarshalActivationResp(r activationGrantResponse) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		b, _ = json.Marshal(activationGrantResponse{})
	}
	return b
}
