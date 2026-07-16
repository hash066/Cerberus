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

	rec, err := e.run(ctx, t)
	if err != nil {
		return 0, err
	}

	e.mu.Lock()
	e.results[h] = rec
	e.mu.Unlock()
	return h, nil
}

// run places and executes one task, keeping CPU-pool accounting balanced on
// every path.
//
// Every acquire is paired with a DEFERRED release, so no branch (or panic) can
// leak a thread. The remote path additionally releases explicitly, before the
// local fallback runs, so the worker's slot is not held while the work is
// actually happening here; the deferred release is then a no-op. Previously the
// remote release was a bare call with no defer, and the local fallback ran with
// no accounting at all.
func (e *PreferRemoteExecutor) run(ctx context.Context, t contract.ComputeTask) (dispatchRecord, error) {
	worker, remote, ok := e.pickWorker(t)

	release := func() {}
	if ok && e.sched != nil {
		if !e.sched.AcquireCPU(worker, scheduler.ThreadsPerTask) {
			ok = false
		} else {
			var once sync.Once
			release = func() {
				once.Do(func() { e.sched.ReleaseCPU(worker, scheduler.ThreadsPerTask) })
			}
			defer release()
		}
	}

	if !ok {
		// No placement (no peers, or every node saturated). Run here as a last
		// resort — same as before — but account it against self rather than
		// silently running unmetered work.
		return e.runLocalAccounted(ctx, t)
	}

	if !remote {
		res, err := e.dispatchLocal(ctx, t)
		if err != nil {
			return dispatchRecord{}, err
		}
		return dispatchRecord{result: res, where: "local"}, nil
	}

	res, where, err := e.dispatchRemote(ctx, worker, t)
	if err == nil {
		return dispatchRecord{result: res, where: where}, nil
	}
	release() // remote is not running it; hand the slot back before falling back
	local, lerr := e.runLocalAccounted(ctx, t)
	if lerr != nil {
		return dispatchRecord{}, fmt.Errorf("remote: %v; local fallback: %w", err, lerr)
	}
	return local, nil
}

// runLocalAccounted runs a task on this node with the local thread accounted in
// the pool. A saturated local pool does NOT refuse the task: this is the
// last-resort path and failing here would drop work that has nowhere else to
// go. The accounting records the over-subscription honestly instead of hiding
// the run entirely, which is what the unaccounted fallback used to do.
func (e *PreferRemoteExecutor) runLocalAccounted(ctx context.Context, t contract.ComputeTask) (dispatchRecord, error) {
	if e.sched != nil && e.fabric != nil {
		self := e.fabric.PeerID()
		if e.sched.AcquireCPU(self, scheduler.ThreadsPerTask) {
			defer e.sched.ReleaseCPU(self, scheduler.ThreadsPerTask)
		}
	}
	res, err := e.dispatchLocal(ctx, t)
	if err != nil {
		return dispatchRecord{}, err
	}
	return dispatchRecord{result: res, where: "local"}, nil
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

// pickWorker selects the node the scheduler's cost model ranks best for t. When
// the scheduler is unset it falls back to round-robin over connected peers
// (legacy behaviour); when every node is infeasible it reports ok=false and the
// caller runs the task locally as a last resort.
//
// This asks BestNodeByCost, NOT BestNode. BestNode ranks on free POOL THREADS
// alone, which is blind to how busy the machines actually are: two idle-pool
// nodes both report "all threads free" no matter that one is pegged at 100%
// host CPU, so they tie and the winner fell out of Go's randomized map
// iteration order. The load signal — NodeTelemetry.Compute.Flops, i.e. peak
// FLOPS scaled by live host utilization (daemon/system.localTelemetry) — is
// only read by the cost model, and the cost model was only reachable through
// Place, which nothing in production calls. So "telemetry-driven placement" was
// real in the cost model and absent from the path that actually dispatches
// work. BestNodeByCost applies that same ranking here, which also means
// dispatch now honours the hard constraints Place always did and this path
// never did: a thermally throttling node, a node below MinVRAM, and a node
// about to sleep are no longer valid targets.
func (e *PreferRemoteExecutor) pickWorker(t contract.ComputeTask) (worker contract.PeerID, remote bool, ok bool) {
	if e.fabric == nil {
		return contract.PeerID{}, false, false
	}
	self := e.fabric.PeerID()

	if e.sched != nil {
		if best, have := e.sched.BestNodeByCost(t, nil); have {
			return best, best != self, true
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
