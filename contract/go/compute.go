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
	Kind             ShardKind
	LayerLo, LayerHi uint32
	TPRank, TPWorld  uint32
}

// Promise is an unresolved future referenced before its producer finishes.
type Promise struct {
	PromiseID []byte
	Producer  PeerID
}

// TensorDType selects the element type of an ActivationFrame payload.
type TensorDType uint8

const (
	TensorDTypeUnspecified TensorDType = iota
	TensorDTypeF32
	TensorDTypeF16
	TensorDTypeBF16
	TensorDTypeI8
)

// CompressionHint names compression on the data-plane activation path.
// CompressionFrontierZK is a documented stub (zk-compressed bundles are Frontier).
const (
	CompressionUnspecified CompressionHint = iota
	CompressionNone
	CompressionZstd
	CompressionLZ4
	CompressionFrontierZK
)

// CompressionHint names compression on the data-plane activation path.
type CompressionHint uint8

// ActivationFrame mirrors compute.proto — tensor bytes plus shape metadata.
// Bulk payload may also ride QUIC directly; this tags what was (or will be) sent.
type ActivationFrame struct {
	Payload     []byte
	Shape       []uint32
	DType       TensorDType
	Compression CompressionHint
	StageIndex  uint32
}

// PipelineStageMeta records one shard placement in a pipeline plan.
type PipelineStageMeta struct {
	StageIndex uint32
	Shard      Shard
	Node       PeerID
}

// ComputeTask mirrors compute.proto / ARCHITECTURE.md §3.4.
type ComputeTask struct {
	TaskID    []byte
	Component []byte // IPLD CID of the WASM component
	Shard     Shard
	Caps      [][]byte // capabilities granted to this task
	Deps      []Promise
	ResultCap []byte
	// Activation is the structured inbound tensor (optional; Caps[1] inline bytes
	// remain valid for v0.1 pipeline demos).
	Activation ActivationFrame
	// PipelineStage is this shard's index in the pipeline plan (optional).
	PipelineStage uint32
}

// InferenceTask is an explicit inference/pipeline dispatch view over ComputeTask.
type InferenceTask struct {
	Task          ComputeTask
	Activation    ActivationFrame
	PipelineStage uint32
	Stages        []PipelineStageMeta
}

// ToComputeTask projects an InferenceTask to the wire ComputeTask shape.
func (it InferenceTask) ToComputeTask() ComputeTask {
	t := it.Task
	if len(it.Activation.Payload) > 0 || len(it.Activation.Shape) > 0 {
		t.Activation = it.Activation
	}
	if it.PipelineStage != 0 {
		t.PipelineStage = it.PipelineStage
	}
	return t
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
