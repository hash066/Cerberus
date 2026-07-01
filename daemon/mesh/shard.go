package mesh

// shard.go adds a minimal request/response for dfs shard placement over the same
// point-to-point libp2p/QUIC stream mechanism compute.go already established: a
// node sends a shardRequest to a peer, the peer runs the requested operation
// (store or fetch a content-addressed shard) against its OWN local dfs shard
// store, and the result comes back over the same stream. This is the transport a
// daemon/system-level ShardStore uses to place SOME shards of a /cer/fs write on
// a remote mesh peer instead of always keeping every shard locally (see
// daemon/system/shardstore.go).
//
// What is REAL here:
//   - The transport is the same libp2p host over QUIC (quic-v1) compute.go uses:
//     encrypted, multiplexed, PeerID-authenticated by the QUIC/TLS handshake. Shard
//     bytes for a v0.1 dfs shard (a fraction of the 1 MiB default chunk size, split
//     across DefaultDataShards+DefaultParityShards) comfortably fit one length-
//     prefixed frame, so — mirroring the guidance that small payloads can ride the
//     request/response frame directly rather than needing a second data-plane
//     hop — the shard bytes travel in-band in the request/response body.
//   - PutShard/GetShard on the SERVING side call directly into that peer's own
//     dfs.ShardStore (its local disk/memory shard storage), so a "put" genuinely
//     stores the bytes on the remote node's own store, and a "get" genuinely reads
//     them back from there — not a local echo.
//
// What is a STUB / not yet here (not faked):
//   - No capability is checked on this path yet: a shard put/get is an
//     engine-internal, node-to-node operation (the dfs engine's own placement
//     mechanics), not a capability-gated resource access from a 9P caller — the
//     9P/capability gate already ran once, at the /cer/fs Open that triggered this
//     write/read. A future hardening step could additionally scope this RPC to
//     mesh peers that hold a fabric-level cap, mirroring compute.go's signed-cap
//     path; that is out of scope for the v1 "does a shard genuinely cross the
//     network" deliverable this file provides.
//   - Large-shard / data-plane-backed transfer: today's dfs default (1 MiB chunk,
//     4 data + 2 parity shards) keeps every shard well under a megabyte, small
//     enough for the in-band request/response frame. A future step could switch a
//     shard above some size threshold to coordinate a daemon/dataplane bulk
//     transfer instead of inlining the bytes here, exactly as compute.go's own doc
//     comment anticipates for larger payloads.

import (
	"context"
	"encoding/json"
	"fmt"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/libp2p/go-libp2p/core/network"
)

// shardProto is the libp2p protocol id for shard put/get requests.
const shardProto = "/cerberus/shard/1.0.0"

// ShardOp identifies which operation a shardRequest performs.
type ShardOp string

const (
	ShardOpPut ShardOp = "put"
	ShardOpGet ShardOp = "get"
)

// shardRequest is the on-wire request frame. CIDBytes is the shard's content
// address (cid.Cid.Bytes()); Data carries the shard payload for a put (empty for
// a get).
type shardRequest struct {
	Op       ShardOp `json:"op"`
	CIDBytes []byte  `json:"cid"`
	Data     []byte  `json:"data,omitempty"`
}

// shardResponse is the on-wire result frame. OK is false with Error set when the
// peer's local ShardStore rejected the operation (e.g. a get miss); Data carries
// the fetched shard bytes on a successful get.
type shardResponse struct {
	OK    bool   `json:"ok"`
	Data  []byte `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// ShardServer is implemented by the local dfs shard store the daemon exposes to
// remote peers over the mesh. It is intentionally the same two methods as
// dfs.ShardStore (PutShard/GetShard take/return raw CID bytes here instead of
// cid.Cid so this package does not need to import daemon/dfs or go-cid on its
// public surface, keeping the mesh/dfs layering one-directional: dfs and
// daemon/system depend on mesh, not the reverse).
type ShardServer interface {
	// PutShard stores shard bytes under the content address cidBytes on this
	// node's own local store.
	PutShard(cidBytes []byte, shard []byte) error
	// GetShard fetches the bytes stored under cidBytes from this node's own local
	// store. A miss returns an error.
	GetShard(cidBytes []byte) ([]byte, error)
}

// ServeShards registers the worker-side handler that lets remote peers store and
// fetch dfs shards on THIS node's local shard store. Call it once, before remote
// requests arrive.
func (f *Fabric) ServeShards(local ShardServer) {
	f.host.SetStreamHandler(shardProto, func(s network.Stream) {
		f.handleShardStream(s, local)
	})
}

func (f *Fabric) handleShardStream(s network.Stream, local ShardServer) {
	ss := newStreamSession(s)
	defer ss.Close()

	raw, err := ss.Recv()
	if err != nil {
		_ = s.Reset()
		return
	}
	var req shardRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		_ = writeShardError(ss, fmt.Sprintf("decode shard request: %v", err))
		return
	}

	switch req.Op {
	case ShardOpPut:
		if err := local.PutShard(req.CIDBytes, req.Data); err != nil {
			_ = writeShardError(ss, err.Error())
			return
		}
		_ = ss.Send(mustMarshalShardResponse(shardResponse{OK: true}))
	case ShardOpGet:
		data, err := local.GetShard(req.CIDBytes)
		if err != nil {
			_ = writeShardError(ss, err.Error())
			return
		}
		_ = ss.Send(mustMarshalShardResponse(shardResponse{OK: true, Data: data}))
	default:
		_ = writeShardError(ss, fmt.Sprintf("unknown shard op %q", req.Op))
	}
}

// RequestPutShard dials peer and asks it to durably store shard bytes under
// cidBytes on ITS OWN local shard store. Returns a non-nil error if the dial, the
// wire round-trip, or the peer's own store rejected the put.
func (f *Fabric) RequestPutShard(ctx context.Context, peer contract.PeerID, cidBytes []byte, shard []byte) error {
	resp, err := f.requestShard(ctx, peer, shardRequest{Op: ShardOpPut, CIDBytes: cidBytes, Data: shard})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("mesh: remote put shard: %s", resp.Error)
	}
	return nil
}

// RequestGetShard dials peer and fetches the shard bytes stored under cidBytes on
// ITS OWN local shard store.
func (f *Fabric) RequestGetShard(ctx context.Context, peer contract.PeerID, cidBytes []byte) ([]byte, error) {
	resp, err := f.requestShard(ctx, peer, shardRequest{Op: ShardOpGet, CIDBytes: cidBytes})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("mesh: remote get shard: %s", resp.Error)
	}
	return resp.Data, nil
}

func (f *Fabric) requestShard(ctx context.Context, peer contract.PeerID, req shardRequest) (shardResponse, error) {
	pid, err := toLibp2pID(peer)
	if err != nil {
		return shardResponse{}, err
	}
	s, err := f.host.NewStream(ctx, pid, shardProto)
	if err != nil {
		return shardResponse{}, fmt.Errorf("mesh: dial shard peer: %w", err)
	}
	ss := newStreamSession(s)
	defer ss.Close()

	body, err := json.Marshal(req)
	if err != nil {
		return shardResponse{}, err
	}
	if err := ss.Send(body); err != nil {
		return shardResponse{}, fmt.Errorf("mesh: send shard request: %w", err)
	}

	raw, err := ss.Recv()
	if err != nil {
		return shardResponse{}, fmt.Errorf("mesh: recv shard response: %w", err)
	}
	var resp shardResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return shardResponse{}, fmt.Errorf("mesh: decode shard response: %w", err)
	}
	return resp, nil
}

func writeShardError(ss *streamSession, msg string) error {
	return ss.Send(mustMarshalShardResponse(shardResponse{OK: false, Error: msg}))
}

func mustMarshalShardResponse(r shardResponse) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		b, _ = json.Marshal(shardResponse{OK: false, Error: "internal: marshal response"})
	}
	return b
}
