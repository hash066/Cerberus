// Package compute wires mesh remote WASM dispatch for the production daemon:
// a prefer-remote contract.Executor for the gateway/CLI and a worker-side
// ServeComputeSigned handler registered during system composition.
package compute

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"strconv"
	"strings"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/scheduler"
	"github.com/hash066/cerberus/daemon/wasm"
	"github.com/ipfs/go-cid"
)

// WorkerConfig configures the mesh compute worker registered on a Fabric.
type WorkerConfig struct {
	Site      string
	Store     *wasm.ContentStore
	Exec      *wasm.Executor
	Revoked   auth.RevocationPredicate
	SeedBytes []byte               // optional default component bytes seeded into Store
	Sched     *scheduler.Scheduler // optional CPU pool accounting on the worker
}

// WireWorker registers ServeComputeSigned and ServeComponentFetch on fab so
// remote peers can dispatch WASM workloads to this node. SeedBytes, when
// non-empty, are content-addressed into Store up front (e.g. hello-shard).
func WireWorker(fab *mesh.Fabric, cfg WorkerConfig) error {
	if fab == nil {
		return fmt.Errorf("compute: nil fabric")
	}
	if cfg.Store == nil {
		return fmt.Errorf("compute: nil content store")
	}
	if cfg.Exec == nil {
		return fmt.Errorf("compute: nil executor")
	}
	if cfg.Site == "" {
		cfg.Site = "local"
	}
	if len(cfg.SeedBytes) > 0 {
		if _, err := cfg.Store.Put(cfg.SeedBytes); err != nil {
			return fmt.Errorf("compute: seed content store: %w", err)
		}
	}

	now := func() int64 { return time.Now().Unix() }
	fab.ServeComputeSigned(
		func(ctx context.Context, task contract.ComputeTask, grant auth.Grant) (contract.ComputeResult, error) {
			// grant is already scope-verified against `want` by the mesh layer
			// (verifySignedCap), which rejects a wrong-resource cap before this
			// handler is ever reached — so there is nothing left to check here.
			_ = grant
			self := fab.PeerID()
			if cfg.Sched != nil {
				if !cfg.Sched.AcquireCPU(self, scheduler.ThreadsPerTask) {
					return contract.ComputeResult{
						TaskID: append([]byte(nil), task.TaskID...),
						OK:     false,
						Error:  "cpu pool saturated",
					}, nil
				}
				defer cfg.Sched.ReleaseCPU(self, scheduler.ThreadsPerTask)
			}
			return runTask(ctx, cfg.Store, cfg.Exec, fab, cfg.Site, task)
		},
		mesh.SelfIssuerResolver,
		now,
		cfg.Revoked,
		contract.RightExec,
		// The gate guards THIS site's mesh-compute resource, which is exactly what
		// PreferRemoteExecutor.mintExecCap issues against. Before this was passed, a
		// cap for any other resource carrying RightExec (mesh-gpu, llama-rpc) ran
		// WASM here.
		mesh.MeshComputeResource(cfg.Site),
	)
	fab.ServeComponentFetch(cfg.Store, mesh.SelfIssuerResolver, now, cfg.Revoked)
	return nil
}

func runTask(
	ctx context.Context,
	store *wasm.ContentStore,
	exec *wasm.Executor,
	fab *mesh.Fabric,
	site string,
	task contract.ComputeTask,
) (contract.ComputeResult, error) {
	taskID := append([]byte(nil), task.TaskID...)
	component, err := resolveComponent(ctx, store, fab, site, task.Component)
	if err != nil {
		return contract.ComputeResult{TaskID: taskID, OK: false, Error: err.Error()}, nil
	}
	promise, derr := exec.Dispatch(ctx, contract.ComputeTask{TaskID: taskID, Component: component})
	if derr != nil {
		return contract.ComputeResult{TaskID: taskID, OK: false, Error: derr.Error()}, nil
	}
	return exec.Resolve(ctx, promise)
}

func resolveComponent(
	ctx context.Context,
	store *wasm.ContentStore,
	fab *mesh.Fabric,
	site string,
	raw []byte,
) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty component")
	}
	if isWasmMagic(raw) {
		c, err := store.Put(raw)
		if err != nil {
			return nil, err
		}
		return store.Get(c)
	}
	c, err := cid.Cast(raw)
	if err != nil {
		// Not a CID — pass through for the executor's default-module path.
		return raw, nil
	}
	b, err := store.Get(c)
	if err == nil {
		return b, nil
	}
	if fab == nil {
		return nil, err
	}
	fetched, ferr := fetchComponentFromPeers(ctx, fab, store, site, c)
	if ferr != nil {
		return nil, fmt.Errorf("%v (peer fetch also failed: %v)", err, ferr)
	}
	return fetched, nil
}

func fetchComponentFromPeers(
	ctx context.Context,
	fab *mesh.Fabric,
	store *wasm.ContentStore,
	site string,
	want cid.Cid,
) ([]byte, error) {
	self := fab.PeerID()
	peers := fab.Peers()
	if len(peers) == 0 {
		return nil, fmt.Errorf("component %s not found locally and no mesh peers are known", want)
	}

	membershipCap, issuerID, err := mintMeshFabricCap(fab, site)
	if err != nil {
		return nil, err
	}

	var errs []string
	for _, p := range peers {
		if p.ID == self {
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		b, err := fab.RequestComponent(fctx, p.ID, want, membershipCap, issuerID)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%x: %v", p.ID[:4], err))
			continue
		}
		if _, perr := store.Put(b); perr != nil {
			errs = append(errs, fmt.Sprintf("%x: fetched but failed to cache: %v", p.ID[:4], perr))
			continue
		}
		return b, nil
	}
	return nil, fmt.Errorf("component %s not found on any of %d known peer(s): %s", want, len(peers), strings.Join(errs, "; "))
}

func mintMeshFabricCap(fab *mesh.Fabric, site string) ([]byte, contract.PeerID, error) {
	signer, err := meshCapSigner(fab)
	if err != nil {
		return nil, contract.PeerID{}, err
	}
	g, err := auth.NewGrant(mesh.MeshFabricResource(site), []contract.Right{contract.RightRead}, nil, time.Hour)
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

func meshCapSigner(fab *mesh.Fabric) (*auth.SignedCap, error) {
	identity := fab.Identity()
	if len(identity) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("compute: mesh identity is not a usable Ed25519 private key (len=%d)", len(identity))
	}
	ks, err := auth.NewMemoryKeyStore(identity.Seed())
	if err != nil {
		return nil, fmt.Errorf("compute: mesh-cap keystore: %w", err)
	}
	return auth.NewSignedCap(ks), nil
}

func isWasmMagic(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x00 && b[1] == 0x61 && b[2] == 0x73 && b[3] == 0x6d
}

// FormatOutput is a test helper that stringifies an i32 shard result.
func FormatOutput(v int32) []byte {
	return []byte(strconv.Itoa(int(v)))
}
