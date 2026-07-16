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
// DURABILITY (this step): the path→Manifest map is now backed by daemon/store
// (bbolt) via BoltMetaStore, mirroring the persistence idiom already established
// in daemon/ledger/ledger.go (a bucket name constant, JSON-marshaled values,
// store.Get/store.Put/store.Keys). MemMetaStore is kept for tests/fixtures that
// do not want an on-disk file, but Compose now wires the durable store so the
// path→Manifest mapping survives a daemon restart. See metastore.go.
//
// PEER SCATTER (this step): shard placement can now put SOME shards on a remote
// mesh peer and fetch them back over a real mesh RPC (daemon/mesh, modeled
// directly on compute.go's request/response pattern), instead of always keeping
// every shard on this node. See shardstore.go for the v1 round-robin placement
// policy; Manifest.Placement records, per shard, "mesh:<base64 PeerID>" so a
// reader on any node dials the owning peer directly (PlacedShardStore).
//
// METADATA REPLICATION (this step): path→Manifest records replicate to mesh
// peers over daemon/mesh/meta.go (capability-gated put/get/list) via
// ReplicatedMetaStore (remotemeta.go), so a file written on node B is visible
// to node A's FSList/FSGet without a separate metadata cluster.
//
// DOCUMENTED STUB (still true): the shard placement policy is a simple
// round-robin over currently-known mesh peers — no load balancing, no
// failure-aware rebalancing, no re-replication on peer loss. That remains a
// later step; see shardstore.go.

package system

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
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

// MetaStore is the path→Manifest seam dfsFSStore uses. MemMetaStore (below) and
// BoltMetaStore (metastore.go) both implement it; Compose wires the durable
// BoltMetaStore, tests/fixtures may use either.
type MetaStore interface {
	// Put durably records the manifest for path (overwriting any prior file at
	// that path).
	Put(path string, man dfs.Manifest) error
	// Get returns the manifest for path, or false if no file was written there.
	Get(path string) (dfs.Manifest, bool)
	// List returns the paths of every stored file, in deterministic order.
	List() ([]string, error)
}

// MemMetaStore maps a /cer/fs path to the dfs Manifest of the file stored there.
// It is the in-memory metadata store: entries do not survive a restart. Kept for
// fast, dependency-free tests/fixtures; Compose wires BoltMetaStore (metastore.go)
// for the durable path.
type MemMetaStore struct {
	mu   sync.RWMutex
	byID map[string]dfs.Manifest
}

// NewMemMetaStore returns an empty in-memory metadata store.
func NewMemMetaStore() *MemMetaStore {
	return &MemMetaStore{byID: map[string]dfs.Manifest{}}
}

// Put records the manifest for path (overwriting any prior file at that path).
func (m *MemMetaStore) Put(path string, man dfs.Manifest) error {
	m.mu.Lock()
	m.byID[path] = man
	m.mu.Unlock()
	return nil
}

// Get returns the manifest for path, or false if no file was written there.
func (m *MemMetaStore) Get(path string) (dfs.Manifest, bool) {
	m.mu.RLock()
	man, ok := m.byID[path]
	m.mu.RUnlock()
	return man, ok
}

// List returns the stored file paths in deterministic (sorted) order.
func (m *MemMetaStore) List() ([]string, error) {
	m.mu.RLock()
	out := make([]string, 0, len(m.byID))
	for p := range m.byID {
		out = append(out, p)
	}
	m.mu.RUnlock()
	sort.Strings(out)
	return out, nil
}

var _ MetaStore = (*MemMetaStore)(nil)

// FSPut stores data as the file at logical path in /cer/fs: it erasure-codes the
// bytes with the dfs engine (scattering shards across mesh peers via the composed
// ShardStore) and durably records the returned Manifest under path. This is the
// synchronous, whole-buffer form the operator CLI (`cerberus fs put`) uses;
// BeginWrite is the streaming data-plane form the 9P namespace uses. The buffer
// is held in memory, so this is intended for modest operator files, not media.
func (s *System) FSPut(path string, data []byte) error {
	if s.fsStore == nil {
		return fmt.Errorf("fs: no /cer/fs store composed")
	}
	man, err := s.fsStore.fs.Put(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("fs put: %w", err)
	}
	return s.fsStore.meta.Put(path, man)
}

// FSGet reconstructs and returns the bytes of the file at path, or an error if
// no file was written there. It resolves path to its Manifest and runs dfs.Get
// (fetching shards from peers as needed, integrity-checked) — it never fabricates
// bytes for a path that was never written.
func (s *System) FSGet(path string) ([]byte, error) {
	if s.fsStore == nil {
		return nil, fmt.Errorf("fs: no /cer/fs store composed")
	}
	man, ok := s.fsStore.meta.Get(path)
	if !ok {
		return nil, fmt.Errorf("fs: no such file: %s", path)
	}
	rc, err := s.fsStore.fs.Get(man)
	if err != nil {
		return nil, fmt.Errorf("fs get: %w", err)
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// FSList returns the logical paths of every file stored in /cer/fs.
func (s *System) FSList() ([]string, error) {
	if s.fsStore == nil {
		return nil, fmt.Errorf("fs: no /cer/fs store composed")
	}
	return s.fsStore.meta.List()
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
	meta   MetaStore
}

// newDFSFSStore builds the /cer/fs backend over the given ShardStore and
// MetaStore. Passing nil for either falls back to the v0.1 in-memory default
// (dfs.NewMemShardStore / NewMemMetaStore), which keeps existing callers (tests,
// fixtures) working unchanged. Compose passes a durable BoltMetaStore and a
// mesh-backed remote ShardStore (see metastore.go / shardstore.go) for the real
// daemon path.
func newDFSFSStore(dp *dataplane.Server, router *sinkRouter, shards dfs.ShardStore, meta MetaStore) (*dfsFSStore, error) {
	if shards == nil {
		shards = dfs.NewMemShardStore()
	}
	if meta == nil {
		meta = NewMemMetaStore()
	}
	fs, err := dfs.New(shards, dfs.DefaultConfig())
	if err != nil {
		return nil, err
	}
	return &dfsFSStore{
		fs:     fs,
		dp:     dp,
		router: router,
		client: dataplane.NewClient(),
		meta:   meta,
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
		return s.meta.Put(path, man)
	})

	return ninep.DataEndpoint{
		Kind:         ninep.EndpointKind(ep.Kind),
		Endpoint:     ep.Addr,
		StreamID:     ep.TransferID,
		Quota:        ep.Quota,
		ServerPeerID: ep.ServerPeerID, // the daemon's own real identity; the caller pins its dial to it.
	}, nil
}

// manifestFor returns the Manifest recorded for path, for white-box tests that
// need to inspect placement without going through a full data-plane read.
func (s *dfsFSStore) manifestFor(path string) (dfs.Manifest, bool) {
	return s.meta.Get(path)
}

// List returns every file recorded in the metadata store, with the size taken
// from each file's Manifest. This is what lets a mount's `ls` show real files
// with real sizes: the namespace's ListChildren draws /cer/fs entries from here
// (through Server.FSList, which filters them to the asking capability first).
//
// A path whose Manifest has vanished between the key listing and the lookup is
// skipped rather than reported at size zero — a file is listed only if its
// metadata is genuinely readable.
func (s *dfsFSStore) List() ([]ninep.FSEntry, error) {
	paths, err := s.meta.List()
	if err != nil {
		return nil, fmt.Errorf("fs list: %w", err)
	}
	out := make([]ninep.FSEntry, 0, len(paths))
	for _, p := range paths {
		man, ok := s.meta.Get(p)
		if !ok {
			continue
		}
		out = append(out, ninep.FSEntry{Path: p, Size: man.TotalBytes})
	}
	return out, nil
}

// Stat returns the entry for exactly path. A path that was never written is
// reported as absent (ErrDenied "no such file", which the 9P layer maps to
// ENOENT) rather than as a zero-byte file.
func (s *dfsFSStore) Stat(path string) (ninep.FSEntry, error) {
	man, ok := s.meta.Get(path)
	if !ok {
		return ninep.FSEntry{}, contract.Errf(contract.ErrDenied, "no such file: "+path)
	}
	return ninep.FSEntry{Path: path, Size: man.TotalBytes}, nil
}

// OpenRead resolves path to its Manifest and returns a random-access reader over
// the erasure-coded file. The reader reconstructs only the chunks each read
// touches (dfs.File), so a mount can serve an arbitrary (offset, count) read
// without materializing the whole file — the reason this exists alongside
// BeginRead, which streams a WHOLE file out over the data plane and cannot be
// driven by a read(2). Shard fetches inside it still go to peers over the mesh.
func (s *dfsFSStore) OpenRead(path string) (ninep.FSReader, error) {
	man, ok := s.meta.Get(path)
	if !ok {
		return nil, contract.Errf(contract.ErrDenied, "no such file: "+path)
	}
	f, err := s.fs.Open(man)
	if err != nil {
		// Return an explicit nil so the interface is nil, not a typed-nil
		// *dfs.File wrapped in a non-nil FSReader.
		return nil, fmt.Errorf("fs open %s: %w", path, err)
	}
	return f, nil
}

var _ ninep.FSStore = (*dfsFSStore)(nil)

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
	defer func() { _ = rc.Close() }()

	// The daemon dials the caller's receiver and streams dfs.Get's bytes to it,
	// authorized by the caller's own inbound grant (recv carries the transfer id,
	// cap and quota the caller registered on its receiver). The file content rides
	// the data plane, not 9P.
	//
	// Pinning note: if the caller told us which PeerID its own receiver presents
	// (recv.ServerPeerID), we pin the dial to it — a MITM impersonating the
	// caller's receiver is rejected before any file byte is sent (see
	// daemon/dataplane/tls.go). When the caller did not supply one (an ad hoc
	// receiver with no durable identity), ServerPeerID stays zero and pinning is
	// skipped; the transfer is then authorized only by the in-band capability
	// check (recv.Cap/recv.Quota) — a documented gap, not a silent one.
	dpEP := dataplane.Endpoint{
		Kind:         dataplane.EndpointKind(recv.Kind),
		Addr:         recv.Endpoint,
		TransferID:   recv.StreamID,
		Cap:          recv.Cap,
		Quota:        recv.Quota,
		ServerPeerID: recv.ServerPeerID,
	}
	return s.client.Send(context.Background(), dpEP, rc, uint64(man.TotalBytes))
}
