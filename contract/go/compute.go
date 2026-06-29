package contract

// ShardKind selects the parallelism strategy for a compute shard.
type ShardKind uint8

const (
	ShardPipeline ShardKind = iota
	ShardTensor
	ShardData
)

// Shard describes a slice of a model/computation.
type Shard struct {
	Kind            ShardKind
	LayerLo, LayerHi uint32
	TPRank, TPWorld  uint32
}

// Promise is an unresolved future referenced before its producer finishes.
type Promise struct {
	PromiseID []byte
	Producer  PeerID
}

// ComputeTask mirrors compute.proto / ARCHITECTURE.md §3.4.
type ComputeTask struct {
	TaskID    []byte
	Component []byte   // IPLD CID of the WASM component
	Shard     Shard
	Caps      [][]byte // capabilities granted to this task
	Deps      []Promise
	ResultCap []byte
}

// ComputeResult is returned for a completed task (the v0.1 demo carries Output).
type ComputeResult struct {
	TaskID []byte
	OK     bool
	Output []byte
	Error  string
}

// CrdtOp mirrors state.proto / ARCHITECTURE.md §3.3.
type CrdtOp struct {
	DocID  []byte
	Actor  PeerID
	Clock  VectorClock
	Domain string // kv | counter | set | log | agent.belief
	Delta  []byte
	Cap    []byte
	Sig    []byte
}

// BeliefConflict is emitted instead of a silent merge for domain "agent.belief".
type BeliefConflict struct {
	DocID      []byte
	Subject    string
	Candidates []BeliefCandidate
	DetectedAt uint64
}

type BeliefCandidate struct {
	Actor PeerID
	Value []byte
	Clock VectorClock
}
