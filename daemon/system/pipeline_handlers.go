// pipeline_handlers.go wires the worker-side pipeline surfaces on a composed
// System: signed mesh compute for split-MLP shards and activation-grant prep for
// inbound tensors over the data plane.
package system

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/dataplane"
	"github.com/hash066/cerberus/daemon/inference"
	"github.com/hash066/cerberus/daemon/mesh"
)

const pipelineActivationQuota = 1 << 20 // 1 MiB ceiling for v0.1 activations

// PipelineResource names the exec resource pipeline shards are scoped to.
func PipelineResource(site string) contract.ResourceRef {
	return contract.ResourceRef{Kind: contract.KindGPU, Path: "/cer/" + site + "/pipeline/exec"}
}

// activationMailbox buffers one inbound activation keyed by transfer id until the
// matching compute shard consumes it.
type activationMailbox struct {
	mu    sync.Mutex
	wait  map[uint64]chan []byte
	ready map[uint64][]byte // activations delivered before take() registered
}

func newActivationMailbox() *activationMailbox {
	return &activationMailbox{wait: map[uint64]chan []byte{}, ready: map[uint64][]byte{}}
}

// Note: there is deliberately no register step. A wait channel exists only while
// a take() is actually blocked on it; deliver() before any taker buffers into
// ready. Pre-registering a wait channel would strand the payload: deliver would
// push into it and remove it from the map, and a later take — unable to find it —
// would wait forever on a fresh channel.
func (m *activationMailbox) deliver(transferID uint64, b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ch, ok := m.wait[transferID]; ok {
		delete(m.wait, transferID)
		ch <- append([]byte(nil), b...)
		return
	}
	m.ready[transferID] = append([]byte(nil), b...)
}

func (m *activationMailbox) take(ctx context.Context, transferID uint64) ([]byte, error) {
	m.mu.Lock()
	if b, ok := m.ready[transferID]; ok {
		delete(m.ready, transferID)
		m.mu.Unlock()
		return b, nil
	}
	ch, ok := m.wait[transferID]
	if !ok {
		ch = make(chan []byte, 1)
		m.wait[transferID] = ch
	}
	m.mu.Unlock()
	select {
	case b := <-ch:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// PipelineWorker holds per-node pipeline worker state (activation mailbox + shard exec).
type PipelineWorker struct {
	mailbox *activationMailbox
	site    string
}

// ActivationRouter dispatches inbound data-plane transfers to one-shot handlers.
type ActivationRouter struct {
	mu       sync.Mutex
	handlers map[uint64]dataplane.Sink
}

// NewActivationRouter returns a router suitable for pipeline activation sinks.
func NewActivationRouter() *ActivationRouter {
	return &ActivationRouter{handlers: map[uint64]dataplane.Sink{}}
}

func (r *ActivationRouter) register(transferID uint64, h dataplane.Sink) {
	r.mu.Lock()
	r.handlers[transferID] = h
	r.mu.Unlock()
}

// Register installs a one-shot handler (exported for e2e harness wiring).
func (r *ActivationRouter) Register(transferID uint64, h dataplane.Sink) {
	r.register(transferID, h)
}

// Route implements dataplane.Sink for Serve.
func (r *ActivationRouter) Route(transferID uint64, rd io.Reader) error {
	r.mu.Lock()
	h, ok := r.handlers[transferID]
	if ok {
		delete(r.handlers, transferID)
	}
	r.mu.Unlock()
	if ok {
		return h(transferID, rd)
	}
	_, err := io.Copy(io.Discard, rd)
	return err
}

// RegisterPipelineWorker installs mesh handlers for pipeline shard execution and
// inbound activation grants. registerSink is called for each inbound activation
// transfer (sinkRouter.register in Compose, ActivationRouter.register in e2e).
// When registerCompute is false, only the activation-grant handler is installed
// (for e2e nodes that multiplex compute in an existing handler).
func RegisterPipelineWorker(
	fab *mesh.Fabric,
	kernel contract.CapKernel,
	dp *dataplane.Server,
	registerSink func(transferID uint64, h dataplane.Sink),
	site string,
	resolveIssuer mesh.IssuerPubResolver,
	registerCompute bool,
) (*PipelineWorker, error) {
	svc := &PipelineWorker{
		mailbox: newActivationMailbox(),
		site:    site,
	}

	fab.ServeActivationGrant(func(quota contract.Quota) (dataplane.Endpoint, error) {
		if quota.Bytes == 0 {
			quota.Bytes = pipelineActivationQuota
		}
		capH, err := kernel.Mint(
			contract.ResourceRef{Kind: contract.KindTopic, Path: "/cer/pipeline/activation"},
			[]contract.Right{contract.RightRead}, nil)
		if err != nil {
			return dataplane.Endpoint{}, err
		}
		transferID := uint64(time.Now().UnixNano())
		ep := dp.RegisterGrant(transferID, capH, quota)
		registerSink(transferID, func(_ uint64, r io.Reader) error {
			b, err := io.ReadAll(io.LimitReader(r, int64(quota.Bytes)))
			if err != nil {
				return err
			}
			svc.mailbox.deliver(transferID, b)
			return nil
		})
		return dataplane.Endpoint{
			Kind:         dataplane.EndpointQUIC,
			Addr:         ep.Addr,
			TransferID:   ep.TransferID,
			Cap:          ep.Cap,
			Quota:        ep.Quota,
			ServerPeerID: ep.ServerPeerID,
		}, nil
	})

	if registerCompute {
		fab.ServeComputeSigned(
			svc.handlePipelineCompute,
			resolveIssuer,
			func() int64 { return time.Now().Unix() },
			nil,
			contract.RightExec,
			// Scope the gate to the pipeline's own resource — the one
			// PipelineRunner.mintExecCap issues against (see pipeline.go). A
			// mesh-compute or mesh-gpu cap must not drive pipeline shards.
			PipelineResource(site),
		)
	}
	return svc, nil
}

func registerPipelineServices(dp *dataplane.Server, fab *mesh.Fabric, kernel contract.CapKernel, router *sinkRouter, site string) error {
	selfPub := fab.Identity().Public().(ed25519.PublicKey)
	var selfID contract.PeerID
	copy(selfID[:], selfPub)
	trusted := map[contract.PeerID]ed25519.PublicKey{selfID: selfPub}
	_, err := RegisterPipelineWorker(
		fab, kernel, dp,
		router.register,
		site,
		PipelineIssuerResolver(fab, trusted),
		true,
	)
	return err
}

func (svc *PipelineWorker) HandleCompute(ctx context.Context, task contract.ComputeTask, grant auth.Grant) (contract.ComputeResult, error) {
	return svc.handlePipelineCompute(ctx, task, grant)
}

func (svc *PipelineWorker) handlePipelineCompute(ctx context.Context, task contract.ComputeTask, grant auth.Grant) (contract.ComputeResult, error) {
	_ = grant
	if task.Shard.Kind != contract.ShardPipeline {
		return failPipelineResult(task.TaskID, "not a pipeline shard task"), nil
	}

	frame, err := pipelineInboundActivation(ctx, task, svc.mailbox)
	if err != nil {
		return failPipelineResult(task.TaskID, err.Error()), nil
	}

	backend := inference.BackendFromComponent(task.Component)
	act, err := inference.ActivationFromContractFrame(frame)
	if err != nil {
		return failPipelineResult(task.TaskID, err.Error()), nil
	}
	outAct, _, err := inference.ForwardActivation(backend, act, task.Shard.LayerLo, task.Shard.LayerHi)
	if err != nil {
		return failPipelineResult(task.TaskID, err.Error()), nil
	}
	outBytes, err := inference.EncodePipelineOutput(outAct)
	if err != nil {
		return failPipelineResult(task.TaskID, err.Error()), nil
	}
	return contract.ComputeResult{
		TaskID: task.TaskID,
		OK:     true,
		Output: outBytes,
	}, nil
}

func pipelineInboundActivation(ctx context.Context, task contract.ComputeTask, mailbox *activationMailbox) (contract.ActivationFrame, error) {
	if frame, ok := contract.TaskActivation(task); ok {
		if err := frame.Validate(); err != nil {
			return contract.ActivationFrame{}, err
		}
		return frame, nil
	}
	if len(task.Deps) > 0 && len(task.Deps[0].PromiseID) >= 8 {
		transferID := binary.LittleEndian.Uint64(task.Deps[0].PromiseID[:8])
		raw, err := mailbox.take(ctx, transferID)
		if err != nil {
			return contract.ActivationFrame{}, err
		}
		frame, derr := inference.UnmarshalPipelineActivation(raw)
		if derr == nil {
			if err := frame.Validate(); err != nil {
				return contract.ActivationFrame{}, err
			}
			return frame, nil
		}
		// Legacy dataplane path: raw split-MLP f32 bytes.
		if _, err := inference.DecodeActivation(raw); err == nil {
			return contract.ActivationFrameFromF32Bytes(inference.SplitMLPShape, raw)
		}
		return contract.ActivationFrame{}, fmt.Errorf("pipeline: dataplane activation: not a valid activation frame or split-MLP vector")
	}
	// Legacy inline caps[1] (v0.1 demos and direct remote tests).
	if len(task.Caps) > 1 && len(task.Caps[1]) > 0 {
		if frame, err := contract.ActivationFrameFromF32Bytes(inference.SplitMLPShape, task.Caps[1]); err == nil {
			return frame, nil
		}
		return contract.ActivationFrame{}, fmt.Errorf("pipeline: caps[1] activation invalid")
	}
	if task.Shard.LayerLo == 0 {
		return contract.ActivationFrameFromF32Bytes(inference.SplitMLPShape, EncodeActivation(SplitMLPDefaultInput))
	}
	return contract.ActivationFrame{}, fmt.Errorf("pipeline shard missing inbound activation")
}

func failPipelineResult(taskID []byte, msg string) contract.ComputeResult {
	return contract.ComputeResult{TaskID: taskID, OK: false, Error: msg}
}

// PipelineIssuerResolver returns an IssuerPubResolver backed by trusted keys plus
// this node's own mesh identity.
func PipelineIssuerResolver(fab *mesh.Fabric, trusted map[contract.PeerID]ed25519.PublicKey) mesh.IssuerPubResolver {
	selfPub := fab.Identity().Public().(ed25519.PublicKey)
	var selfID contract.PeerID
	copy(selfID[:], selfPub)
	return func(issuer contract.PeerID) (ed25519.PublicKey, bool) {
		if issuer == selfID {
			return selfPub, true
		}
		if trusted == nil {
			return nil, false
		}
		pub, ok := trusted[issuer]
		return pub, ok
	}
}
