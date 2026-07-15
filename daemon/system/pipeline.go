// pipeline.go orchestrates multi-shard pipeline execution: placement via the
// scheduler, per-shard compute over signed mesh RPC, and activation handoff over
// the QUIC data plane between stages on different nodes.
package system

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/dataplane"
	"github.com/hash066/cerberus/daemon/inference"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/scheduler"
)

// PipelineStageResult records one shard execution in a pipeline run.
type PipelineStageResult struct {
	Shard    contract.Shard
	Node     contract.PeerID
	OK       bool
	Error    string
	Remote   bool
	Duration time.Duration
}

// PipelineResult is the outcome of RunPipeline.
type PipelineResult struct {
	OK      bool
	Output  []byte
	Error   string
	Plan    contract.Plan
	Stages  []PipelineStageResult
	Backend string // "cpu-software", "llamacpp"/"llamacpp-mock", or "mlx"/"mlx-mock"
}

// PipelineRunner wires scheduler placement to mesh compute and dataplane activations.
type PipelineRunner struct {
	Sched     *scheduler.Scheduler
	Fabric    *mesh.Fabric
	DataPlane *dataplane.Client
	Self      contract.PeerID
	Site      string
	Signer    *auth.SignedCap   // optional; defaults to mesh-identity signer
	Backend   inference.Backend // zero = cpu-software (splitmlp fixture)
}

// NewPipelineRunner builds a runner over an already-composed System.
func NewPipelineRunner(sys *System, fab *mesh.Fabric, trusted map[contract.PeerID][]byte) (*PipelineRunner, error) {
	return NewPipelineRunnerFromFabric(fab, sys.Scheduler, trusted)
}

// NewPipelineRunnerFromFabric builds a runner without requiring the full System
// wrapper (used by the pipeline e2e harness).
func NewPipelineRunnerFromFabric(fab *mesh.Fabric, sched *scheduler.Scheduler, trusted map[contract.PeerID][]byte) (*PipelineRunner, error) {
	if fab == nil || sched == nil {
		return nil, fmt.Errorf("pipeline: fabric and scheduler required")
	}
	client, err := dataplane.NewClientWithIdentity(fab.Identity())
	if err != nil {
		return nil, err
	}
	trustedKeys := map[contract.PeerID]ed25519.PublicKey{}
	for id, pub := range trusted {
		if len(pub) == ed25519PubSize {
			trustedKeys[id] = ed25519.PublicKey(append([]byte(nil), pub...))
		}
	}
	_ = trustedKeys // reserved for future peer-trust wiring on the orchestrator
	site := "local"
	return &PipelineRunner{
		Sched:     sched,
		Fabric:    fab,
		DataPlane: client,
		Self:      fab.PeerID(),
		Site:      site,
	}, nil
}

const ed25519PubSize = 32

// RunPipeline places shards, executes them in layer order, and returns the final
// activation bytes. input may be nil to use SplitMLPDefaultInput on the first shard.
func (r *PipelineRunner) RunPipeline(ctx context.Context, taskID []byte, shards []contract.Shard, input []byte) (PipelineResult, error) {
	return r.RunPipelineWithHook(ctx, taskID, shards, input, nil)
}

// RunPipelineWithHook is like RunPipeline but invokes hook after each successful
// stage with the activation bytes produced by that stage (used for SSE streaming).
func (r *PipelineRunner) RunPipelineWithHook(ctx context.Context, taskID []byte, shards []contract.Shard, input []byte, hook PipelineStageHook) (PipelineResult, error) {
	if len(shards) == 0 {
		return PipelineResult{}, fmt.Errorf("pipeline: no shards")
	}
	backend := r.pipelineBackend()
	inputFrame, stage, err := inference.ResolvePipelineInput(input)
	if err != nil {
		return PipelineResult{}, fmt.Errorf("pipeline: input: %w", err)
	}
	inputFrame.StageIndex = 0

	// Seed scheduler with this node; callers (e2e harness, RPC) may add peers explicitly.
	r.Sched.UpdateNode(localTelemetry(r.Self))

	plan, err := r.Sched.PlacePipeline(taskID, shards)
	if err != nil {
		return PipelineResult{Error: err.Error()}, err
	}

	ordered := append([]contract.Placement(nil), plan.Placements...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Shard.LayerLo < ordered[j].Shard.LayerLo
	})

	var (
		result     PipelineResult
		stageFrame = inputFrame
	)
	result.Plan = plan
	result.Backend = inference.ReportedBackend(backend)

	for i, placement := range ordered {
		start := time.Now()
		stageRes := PipelineStageResult{Shard: placement.Shard, Node: placement.Node}

		var (
			activationTransferID uint64
			remoteFrame          = stageFrame
		)
		if placement.Node != r.Self {
			wire, err := inference.MarshalPipelineActivation(stageFrame)
			if err != nil {
				result.Error = fmt.Sprintf("stage %d activation marshal: %v", i, err)
				result.Stages = append(result.Stages, stageRes)
				return result, err
			}
			ep, err := r.Fabric.RequestActivationGrant(ctx, placement.Node, uint64(len(wire)))
			if err != nil {
				result.Error = fmt.Sprintf("stage %d activation grant: %v", i, err)
				result.Stages = append(result.Stages, stageRes)
				return result, err
			}
			if err := r.DataPlane.SendBytes(ctx, ep, wire); err != nil {
				result.Error = fmt.Sprintf("stage %d activation send: %v", i, err)
				result.Stages = append(result.Stages, stageRes)
				return result, err
			}
			activationTransferID = ep.TransferID
			remoteFrame = inference.ActivationMetadata(stageFrame)
		}

		out, remote, err := r.runShard(ctx, taskID, placement, stage, remoteFrame, i, activationTransferID, backend)
		stageRes.Duration = time.Since(start)
		stageRes.Remote = remote
		if err != nil {
			stageRes.Error = err.Error()
			result.Stages = append(result.Stages, stageRes)
			result.Error = err.Error()
			return result, err
		}
		stageRes.OK = out.OK
		if !out.OK {
			stageRes.Error = out.Error
			result.Stages = append(result.Stages, stageRes)
			result.Error = out.Error
			return result, fmt.Errorf("pipeline: stage %d failed: %s", i, out.Error)
		}
		stage = append([]byte(nil), out.Output...)
		if wrapped, err := inference.WrapStageOutput(stage, stageFrame); err == nil {
			stageFrame = wrapped
			stageFrame.StageIndex = uint32(i + 1)
		}
		stageRes.OK = true
		result.Stages = append(result.Stages, stageRes)
		if hook != nil {
			hook(stageRes, stage)
		}
	}

	result.OK = true
	result.Output = stage
	return result, nil
}

func (r *PipelineRunner) pipelineBackend() inference.Backend {
	if r.Backend == "" {
		return inference.BackendCPUSoftware
	}
	return r.Backend
}

func (r *PipelineRunner) runShard(
	ctx context.Context,
	taskID []byte,
	placement contract.Placement,
	localInput []byte,
	localFrame contract.ActivationFrame,
	stageIndex int,
	activationTransferID uint64,
	backend inference.Backend,
) (contract.ComputeResult, bool, error) {
	shardTaskID := append(append([]byte(nil), taskID...), byte(placement.Shard.LayerLo), byte(placement.Shard.LayerHi))

	if placement.Node == r.Self {
		act, err := inference.DecodePipelineActivation(localInput, localFrame)
		if err != nil {
			return contract.ComputeResult{}, false, err
		}
		outAct, _, err := inference.ForwardActivation(backend, act, placement.Shard.LayerLo, placement.Shard.LayerHi)
		if err != nil {
			return contract.ComputeResult{}, false, err
		}
		outBytes, err := inference.EncodePipelineOutput(outAct)
		if err != nil {
			return contract.ComputeResult{}, false, err
		}
		return contract.ComputeResult{TaskID: shardTaskID, OK: true, Output: outBytes}, false, nil
	}

	capH, env, issuerID, err := r.pipelineExecCap()
	if err != nil {
		return contract.ComputeResult{}, false, err
	}
	localFrame.StageIndex = uint32(stageIndex)
	caps := [][]byte{env}
	// Inline activation only for local execution; remote stages receive structured
	// metadata on the control plane and marshaled ActivationFrame bytes on QUIC.
	if activationTransferID == 0 {
		caps = append(caps, localInput)
	}
	task := contract.ComputeTask{
		TaskID:        shardTaskID,
		Component:     inference.ComponentTag(backend),
		Shard:         placement.Shard,
		Caps:          caps,
		Activation:    localFrame,
		PipelineStage: uint32(stageIndex),
	}
	if activationTransferID != 0 {
		var tid [8]byte
		binary.LittleEndian.PutUint64(tid[:], activationTransferID)
		task.Deps = []contract.Promise{{PromiseID: tid[:]}}
	}

	res, err := r.Fabric.RequestComputeSigned(ctx, placement.Node, task, issuerID, capH)
	return res, true, err
}

// DefaultSplitMLPShards splits the 4-layer fixture into two pipeline shards:
// layers 0-1 and layers 2-3.
func DefaultSplitMLPShards() []contract.Shard {
	return []contract.Shard{
		{Kind: contract.ShardPipeline, LayerLo: 0, LayerHi: 1},
		{Kind: contract.ShardPipeline, LayerLo: 2, LayerHi: 3},
	}
}

func (r *PipelineRunner) pipelineExecCap() (contract.CapHandle, []byte, contract.PeerID, error) {
	signer := r.Signer
	if signer == nil {
		var err error
		signer, err = NewShardCapSigner(r.Fabric.Identity())
		if err != nil {
			return 0, nil, contract.PeerID{}, err
		}
	}
	issuerID, err := signer.IssuerPeerID()
	if err != nil {
		return 0, nil, contract.PeerID{}, err
	}
	g, err := auth.NewGrant(PipelineResource(r.Site), []contract.Right{contract.RightExec}, nil, time.Hour)
	if err != nil {
		return 0, nil, contract.PeerID{}, err
	}
	env, err := signer.Issue(g)
	if err != nil {
		return 0, nil, contract.PeerID{}, err
	}
	return contract.CapHandle(1), env, issuerID, nil
}

// FormatPeerID hex-encodes a peer id for CLI output.
func FormatPeerID(p contract.PeerID) string {
	return hex.EncodeToString(p[:])
}
