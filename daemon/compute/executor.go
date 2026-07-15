package compute

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/scheduler"
	"github.com/hash066/cerberus/daemon/wasm"
	"github.com/ipfs/go-cid"
)

const remoteDispatchTimeout = 15 * time.Second

// PreferRemoteConfig configures an executor that dispatches over the mesh when
// worker peers are connected, falling back to Local otherwise.
type PreferRemoteConfig struct {
	Fabric *mesh.Fabric
	Site   string
	Local  contract.Executor
	Store  *wasm.ContentStore   // used to content-address wasm bytes before remote dispatch
	Sched  *scheduler.Scheduler // optional CPU pool for least-loaded placement
}

// PreferRemoteExecutor implements contract.Executor. It tries mesh remote
// compute first when at least one peer (other than self) is connected.
type PreferRemoteExecutor struct {
	fabric *mesh.Fabric
	site   string
	local  contract.Executor
	store  *wasm.ContentStore
	sched  *scheduler.Scheduler

	mu      sync.Mutex
	next    uint64
	results map[contract.PromiseHandle]dispatchRecord
}

type dispatchRecord struct {
	result contract.ComputeResult
	where  string
}

// NewPreferRemoteExecutor builds a mesh-preferring executor. Fabric may be nil
// (always local). Store may be nil when tasks always carry resolvable CIDs.
func NewPreferRemoteExecutor(cfg PreferRemoteConfig) *PreferRemoteExecutor {
	site := cfg.Site
	if site == "" {
		site = "local"
	}
	return &PreferRemoteExecutor{
		fabric:  cfg.Fabric,
		site:    site,
		local:   cfg.Local,
		store:   cfg.Store,
		sched:   cfg.Sched,
		results: map[contract.PromiseHandle]dispatchRecord{},
	}
}

// LastWhere returns where the most recently resolved promise ran ("local" or a
// peer id hex). Used by gateway workload history hooks.
func (e *PreferRemoteExecutor) LastWhere(p contract.PromiseHandle) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if rec, ok := e.results[p]; ok {
		return rec.where
	}
	return ""
}

func (e *PreferRemoteExecutor) Dispatch(ctx context.Context, t contract.ComputeTask) (contract.PromiseHandle, error) {
	e.mu.Lock()
	e.next++
	h := contract.PromiseHandle(e.next)
	e.mu.Unlock()

	var rec dispatchRecord
	worker, remote, ok := e.pickWorker()
	if ok {
		if e.sched != nil && !e.sched.AcquireCPU(worker, scheduler.ThreadsPerTask) {
			ok = false
		}
	}
	if ok {
		if remote {
			res, where, err := e.dispatchRemote(ctx, worker, t)
			if e.sched != nil {
				e.sched.ReleaseCPU(worker, scheduler.ThreadsPerTask)
			}
			if err == nil {
				rec = dispatchRecord{result: res, where: where}
			} else {
				localRes, localErr := e.dispatchLocal(ctx, t)
				if localErr != nil {
					return 0, fmt.Errorf("remote: %v; local fallback: %w", err, localErr)
				}
				rec = dispatchRecord{result: localRes, where: "local"}
			}
		} else {
			if e.sched != nil {
				defer e.sched.ReleaseCPU(worker, scheduler.ThreadsPerTask)
			}
			localRes, err := e.dispatchLocal(ctx, t)
			if err != nil {
				return 0, err
			}
			rec = dispatchRecord{result: localRes, where: "local"}
		}
	} else {
		localRes, err := e.dispatchLocal(ctx, t)
		if err != nil {
			return 0, err
		}
		rec = dispatchRecord{result: localRes, where: "local"}
	}

	e.mu.Lock()
	e.results[h] = rec
	e.mu.Unlock()
	return h, nil
}

func (e *PreferRemoteExecutor) Resolve(_ context.Context, p contract.PromiseHandle) (contract.ComputeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, ok := e.results[p]
	if !ok {
		return contract.ComputeResult{}, contract.Errf(contract.ErrDenied, "unknown promise")
	}
	return rec.result, nil
}

func (e *PreferRemoteExecutor) dispatchLocal(ctx context.Context, t contract.ComputeTask) (contract.ComputeResult, error) {
	if e.local == nil {
		return contract.ComputeResult{}, fmt.Errorf("no local executor")
	}
	promise, err := e.local.Dispatch(ctx, t)
	if err != nil {
		return contract.ComputeResult{}, err
	}
	return e.local.Resolve(ctx, promise)
}

// pickWorker selects the node with the most free CPU in the cluster pool. When
// the scheduler is unset or every node is saturated, it falls back to round-robin
// over connected peers (legacy behaviour).
func (e *PreferRemoteExecutor) pickWorker() (worker contract.PeerID, remote bool, ok bool) {
	if e.fabric == nil {
		return contract.PeerID{}, false, false
	}
	self := e.fabric.PeerID()

	if e.sched != nil {
		if best, _, have := e.sched.BestNode(nil); have {
			if best == self {
				return self, false, true
			}
			return best, true, true
		}
		return contract.PeerID{}, false, false
	}

	candidates := make([]contract.PeerID, 0)
	for _, p := range e.fabric.Peers() {
		if p.ID != self && p.ID != (contract.PeerID{}) {
			candidates = append(candidates, p.ID)
		}
	}
	if len(candidates) == 0 {
		return contract.PeerID{}, false, false
	}
	e.mu.Lock()
	idx := e.next % uint64(len(candidates))
	e.next++
	e.mu.Unlock()
	return candidates[idx], true, true
}

func (e *PreferRemoteExecutor) dispatchRemote(ctx context.Context, worker contract.PeerID, t contract.ComputeTask) (contract.ComputeResult, string, error) {
	componentCID, err := e.componentCID(t.Component)
	if err != nil {
		return contract.ComputeResult{}, "", err
	}

	env, issuerID, err := e.mintExecCap()
	if err != nil {
		return contract.ComputeResult{}, "", err
	}

	task := contract.ComputeTask{
		TaskID:    append([]byte(nil), t.TaskID...),
		Component: componentCID.Bytes(),
		Caps:      [][]byte{env},
	}

	rctx, cancel := context.WithTimeout(ctx, remoteDispatchTimeout)
	defer cancel()

	res, err := e.fabric.RequestComputeSigned(rctx, worker, task, issuerID, 0)
	where := hex.EncodeToString(worker[:])
	return res, where, err
}

func (e *PreferRemoteExecutor) componentCID(raw []byte) (cid.Cid, error) {
	if len(raw) == 0 {
		return cid.Undef, fmt.Errorf("empty component")
	}
	if isWasmMagic(raw) {
		if e.store == nil {
			c, err := wasm.ComponentCID(raw)
			if err != nil {
				return cid.Undef, err
			}
			return c, nil
		}
		return e.store.Put(raw)
	}
	if c, err := cid.Cast(raw); err == nil {
		return c, nil
	}
	// Gateway may pass a model name or multibase CID string.
	if c, err := cid.Decode(string(raw)); err == nil {
		return c, nil
	}
	if e.store != nil {
		if c, err := wasm.ComponentCID(raw); err == nil {
			if e.store.Has(c) {
				return c, nil
			}
		}
	}
	return cid.Undef, fmt.Errorf("cannot resolve component %q to a CID", string(raw))
}

func (e *PreferRemoteExecutor) mintExecCap() ([]byte, contract.PeerID, error) {
	signer, err := meshCapSigner(e.fabric)
	if err != nil {
		return nil, contract.PeerID{}, err
	}
	g, err := auth.NewGrant(
		mesh.MeshComputeResource(e.site),
		[]contract.Right{contract.RightExec},
		nil,
		time.Hour,
	)
	if err != nil {
		return nil, contract.PeerID{}, err
	}
	env, err := signer.Issue(g)
	if err != nil {
		return nil, contract.PeerID{}, err
	}
	issuerID, err := signer.IssuerPeerID()
	if err != nil {
		return nil, contract.PeerID{}, err
	}
	return env, issuerID, nil
}

// DispatchRemote sends a task to an explicit worker peer using the same signed
// capability pattern as PreferRemoteExecutor. Used by DaemonRPC.Run --on.
func DispatchRemote(
	ctx context.Context,
	fab *mesh.Fabric,
	site string,
	worker contract.PeerID,
	task contract.ComputeTask,
	store *wasm.ContentStore,
) (contract.ComputeResult, error) {
	if fab == nil {
		return contract.ComputeResult{}, fmt.Errorf("mesh not composed")
	}
	ex := NewPreferRemoteExecutor(PreferRemoteConfig{Fabric: fab, Site: site, Store: store})
	componentCID, err := ex.componentCID(task.Component)
	if err != nil {
		return contract.ComputeResult{}, err
	}
	env, issuerID, err := ex.mintExecCap()
	if err != nil {
		return contract.ComputeResult{}, err
	}
	remoteTask := contract.ComputeTask{
		TaskID:    append([]byte(nil), task.TaskID...),
		Component: componentCID.Bytes(),
		Caps:      [][]byte{env},
	}
	rctx, cancel := context.WithTimeout(ctx, remoteDispatchTimeout)
	defer cancel()
	return fab.RequestComputeSigned(rctx, worker, remoteTask, issuerID, 0)
}

var _ contract.Executor = (*PreferRemoteExecutor)(nil)
