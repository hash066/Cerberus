// /cer/fs composition — bind the distributed filesystem engine (daemon/dfs) and
// the QUIC data plane (daemon/dataplane) behind the 9P namespace's FSStore seam.
//
// This is the seam that makes /cer/fs real end-to-end. The 9P namespace
// (daemon/ninep) checks the capability and calls into FSStore; this file owns all
// the dfs + data-plane mechanics so the namespace couples to neither.
//
// Invariant (ARCHITECTURE.md §3.5, vertical 04 §3): 9P read/write carries no bulk
// bytes. A write streams the file bytes over the data plane INTO dfs.Put; a read
// streams dfs.Get's reconstructed bytes back over the data plane. The 9P layer
// only mints the grant.
//
// Data-plane direction (the data plane is send-only: a client sends to a server):
//   - WRITE: the caller sends the file bytes to the daemon's data-plane receiver;
//     the routing sink pipes them straight into dfs.Put and records the Manifest.
//   - READ: the daemon holds the bytes, so it is the SENDER — it dials the
//     receiver the caller supplied and streams dfs.Get's output to it.
//
// DOCUMENTED STUBS (later steps, not faked):
//   - The path→Manifest map (MemMetaStore) is in-memory. A durable, transactional
//     metadata store (names, sizes, concurrent-write locks — vertical 04 §3) is
//     the next step.
//   - Shard placement is dfs's MemShardStore (single node). Peer scatter over the
//     data plane is the next step (dfs.ShardStore + Manifest.Placement).

package system

import (
	"context"
	"io"
	"sync"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/dataplane"
	"github.com/hash066/cerberus/daemon/dfs"
	"github.com/hash066/cerberus/daemon/ninep"
)

// fsWriteQuota is the byte ceiling minted for a /cer/fs write grant. A file may be
// up to this many bytes for v0.1; a real deployment derives it from the caller's
// capability caveats and free-space accounting. Labelled here as a fixed v0.1
// ceiling, not a silent unbounded grant.
const fsWriteQuota uint64 = 256 << 20 // 256 MiB

// MemMetaStore maps a /cer/fs path to the dfs Manifest of the file stored there.
// It is the in-memory metadata store for v0.1.
//
// DOCUMENTED STUB: a durable, transactional metadata store with concurrent-write
// locks (vertical 04 §3) is the next step. This map loses its entries on restart
// and offers no cross-write isolation beyond a mutex.
type MemMetaStore struct {
	mu   sync.RWMutex
	byID map[string]dfs.Manifest
}

// NewMemMetaStore returns an empty in-memory metadata store.
func NewMemMetaStore() *MemMetaStore {
	return &MemMetaStore{byID: map[string]dfs.Manifest{}}
}

// Put records the manifest for path (overwriting any prior file at that path).
func (m *MemMetaStore) Put(path string, man dfs.Manifest) {
	m.mu.Lock()
	m.byID[path] = man
	m.mu.Unlock()
}

// Get returns the manifest for path, or false if no file was written there.
func (m *MemMetaStore) Get(path string) (dfs.Manifest, bool) {
	m.mu.RLock()
	man, ok := m.byID[path]
	m.mu.RUnlock()
	return man, ok
}

// sinkRouter is the daemon's single data-plane Sink. The data-plane server calls
// it with (transferID, reader) for every authorized inbound transfer; the router
// dispatches to the handler registered for that transferID (an FS write pipes the
// reader into dfs.Put). A transfer with no registered handler is drained to
// discard within its quota (the default device/VRAM behaviour), so wiring the FS
// router does not change how non-FS transfers are handled.
type sinkRouter struct {
	mu       sync.Mutex
	handlers map[uint64]dataplane.Sink
}

func newSinkRouter() *sinkRouter {
	return &sinkRouter{handlers: map[uint64]dataplane.Sink{}}
}

// register installs a one-shot handler for transferID; route removes it after it
// fires so a transfer id is never reused across handlers.
func (r *sinkRouter) register(transferID uint64, h dataplane.Sink) {
	r.mu.Lock()
	r.handlers[transferID] = h
	r.mu.Unlock()
}

func (r *sinkRouter) take(transferID uint64) (dataplane.Sink, bool) {
	r.mu.Lock()
	h, ok := r.handlers[transferID]
	if ok {
		delete(r.handlers, transferID)
	}
	r.mu.Unlock()
	return h, ok
}

// route is the dataplane.Sink installed on the daemon's data-plane server.
func (r *sinkRouter) route(transferID uint64, rd io.Reader) error {
	if h, ok := r.take(transferID); ok {
		return h(transferID, rd)
	}
	// No FS (or other) handler for this transfer: drain within the quota the
	// server already enforces. This preserves the prior nil-sink behaviour for
	// device/VRAM ctl grants.
	_, err := io.Copy(io.Discard, rd)
	return err
}

// dfsFSStore backs ninep.FSStore with the real dfs engine + data plane. It is the
// concrete implementation the namespace calls after a capability check.
type dfsFSStore struct {
	fs     *dfs.FS
	dp     *dataplane.Server
	router *sinkRouter
	client *dataplane.Client
	meta   *MemMetaStore
}

// newDFSFSStore builds the /cer/fs backend over an in-memory shard store (v0.1).
//
// DOCUMENTED STUB: MemShardStore keeps every shard in this process's memory. Peer
// scatter (placing shards on remote peers' free space and fetching over the data
// plane) is the next step — dfs already carries the placement hints in the
// Manifest for it.
func newDFSFSStore(dp *dataplane.Server, router *sinkRouter) (*dfsFSStore, error) {
	fs, err := dfs.New(dfs.NewMemShardStore(), dfs.DefaultConfig())
	if err != nil {
		return nil, err
	}
	return &dfsFSStore{
		fs:     fs,
		dp:     dp,
		router: router,
		client: dataplane.NewClient(),
		meta:   NewMemMetaStore(),
	}, nil
}

// BeginWrite registers a capability-bound data-plane transfer (under the
// namespace-assigned transferID) whose received bytes stream straight into
// dfs.Put; on a clean transfer the resulting Manifest is recorded keyed by path.
// The returned endpoint is where the caller SENDS the file bytes. The bytes never
// touch 9P.
func (s *dfsFSStore) BeginWrite(path string, cap contract.CapHandle, transferID uint64) (ninep.DataEndpoint, error) {
	quota := contract.Quota{Bytes: fsWriteQuota}
	ep := s.dp.RegisterGrant(transferID, cap, quota)

	// The per-transfer sink: the data-plane server streams the reader (the file
	// bytes, bounded by the quota it enforces) directly into dfs.Put. dfs chunks,
	// erasure-codes, content-addresses and scatters the shards; the Manifest it
	// returns is the file's metadata, recorded under the path.
	s.router.register(transferID, func(_ uint64, r io.Reader) error {
		man, err := s.fs.Put(r)
		if err != nil {
			return err
		}
		s.meta.Put(path, man)
		return nil
	})

	return ninep.DataEndpoint{
		Kind:     ninep.EndpointKind(ep.Kind),
		Endpoint: ep.Addr,
		StreamID: ep.TransferID,
		Quota:    ep.Quota,
	}, nil
}

// BeginRead resolves path to its Manifest, reconstructs the file with dfs.Get,
// and streams the bytes over the data plane to the receiver the caller supplied
// in recv (the daemon is the sender for a read). An unknown path is denied — a
// read of a path never written fails, it does not fabricate bytes. It blocks
// until the file has been delivered over the data plane (or the transfer failed).
func (s *dfsFSStore) BeginRead(path string, _ contract.CapHandle, recv ninep.RecvEndpoint) error {
	man, ok := s.meta.Get(path)
	if !ok {
		return contract.Errf(contract.ErrDenied, "no such file: "+path)
	}
	rc, err := s.fs.Get(man)
	if err != nil {
		return err
	}
	defer rc.Close()

	// The daemon dials the caller's receiver and streams dfs.Get's bytes to it,
	// authorized by the caller's own inbound grant (recv carries the transfer id,
	// cap and quota the caller registered on its receiver). The file content rides
	// the data plane, not 9P.
	dpEP := dataplane.Endpoint{
		Kind:       dataplane.EndpointKind(recv.Kind),
		Addr:       recv.Endpoint,
		TransferID: recv.StreamID,
		Cap:        recv.Cap,
		Quota:      recv.Quota,
	}
	return s.client.Send(context.Background(), dpEP, rc, uint64(man.TotalBytes))
}
