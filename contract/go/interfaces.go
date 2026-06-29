package contract

import "context"

// These interfaces are the cross-lane seams. The owning lane implements the
// real version; other lanes depend only on the interface and use a stub until
// integration (see CONTRACT.md §4).

// CapKernel is implemented by core/ocap (lane A). It is the spine: every
// cross-boundary action is authorized here. Stubbed by B and C standalone.
type CapKernel interface {
	Mint(r ResourceRef, rights []Right, caveats []Caveat) (CapHandle, error)
	Attenuate(parent CapHandle, dropRights []Right, addCaveats []Caveat) (CapHandle, error)
	Verify(h CapHandle, req Request, nowUnix int64) error
	Revoke(h CapHandle) error
	IsRevoked(h CapHandle) bool
}

// Fabric is implemented by daemon/mesh (lane B). Control-plane transport:
// Zenoh intra-site + libp2p inter-site. Every pub/sub presents a topic cap.
type Fabric interface {
	Publish(ctx context.Context, key string, msg []byte, cap CapHandle) error
	Subscribe(ctx context.Context, keyExpr string, cap CapHandle) (<-chan Sample, error)
	Dial(peer PeerID) (Session, error)
	Peers() []PeerInfo
}

type Sample struct {
	Key     string
	Payload []byte
}

type PeerInfo struct {
	ID   PeerID
	Addr string
}

// Session is a point-to-point stream to a peer (QUIC under the hood).
type Session interface {
	Send(b []byte) error
	Recv() ([]byte, error)
	Close() error
}

// TelemetrySource is implemented by daemon/telemetry (lane B); consumed by the
// scheduler (lane A) and lifecycle/tray (lane C).
type TelemetrySource interface {
	Latest(peer PeerID) (NodeTelemetry, bool)
	Stream(ctx context.Context) (<-chan NodeTelemetry, error)
}

// Executor is implemented by core/runtime (lane A): run a WASM component task,
// returning a promise that resolves to a result. Promise pipelining lives here.
type Executor interface {
	Dispatch(ctx context.Context, t ComputeTask) (PromiseHandle, error)
	Resolve(ctx context.Context, p PromiseHandle) (ComputeResult, error)
}

type PromiseHandle uint64

// Scheduler is implemented by daemon/scheduler (lane A): decide placement.
type Scheduler interface {
	Place(t ComputeTask) (Plan, error)
	Reroute(taskID []byte, lost PeerID) (Plan, error)
}

// Plan is a placement decision: each shard on a node, plus hot standbys.
type Plan struct {
	TaskID     []byte
	Placements []Placement
	Standbys   []Placement
}

type Placement struct {
	Shard Shard
	Node  PeerID
	Caps  [][]byte
}

// CrdtEngine is implemented by core/crdt (lane A); consumed by identity (B)
// for the revocation OR-set and by lifecycle (C) for checkpoints.
type CrdtEngine interface {
	Apply(op CrdtOp) error
	Merge(remote []CrdtOp) (conflicts []BeliefConflict, err error)
	Snapshot(docID []byte) ([]byte, error)
	Checkpoint(docID []byte) ([]byte, error)
}
