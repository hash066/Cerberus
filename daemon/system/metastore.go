// metastore.go is the durable /cer/fs metadata store: BoltMetaStore persists the
// path→dfs.Manifest mapping in daemon/store (bbolt), so a daemon restart does not
// lose which files exist or where their shards are.
//
// This mirrors the persistence idiom already established in
// daemon/ledger/ledger.go (a single bucket name constant, JSON-marshaled values,
// store.Get/store.Put/store.Keys) rather than inventing a new one: a bucket per
// logical store, keys namespaced within it, JSON values, and an Open that loads
// nothing eagerly (Get/Put read/write straight through to bbolt so there is no
// separate durability step to remember to call).
package system

import (
	"encoding/json"
	"sort"
	"sync"

	"github.com/hash066/cerberus/daemon/dfs"
	"github.com/hash066/cerberus/daemon/store"
)

// metaBucket is the bbolt bucket name for the /cer/fs path→Manifest mapping.
const metaBucket = "fs.meta"

// BoltMetaStore is the durable, transactional path→Manifest metadata store
// (vertical 04 §3): every Put is a synchronous bbolt write (fsync'd by bbolt's
// default Update transaction) before it returns, so a Manifest recorded here is
// still there after the daemon process restarts against the same store file.
//
// Concurrent-write isolation: bbolt serializes all Update transactions against
// one file, so two concurrent Puts (even to different paths) cannot interleave
// partial writes; the in-process mutex additionally serializes the read-modify
// pattern callers of this type rely on (Get-then-Put) so a caller doing that
// two-step is also safe across goroutines within this process.
type BoltMetaStore struct {
	s  *store.Store
	mu sync.Mutex
}

// NewBoltMetaStore wraps an already-open *store.Store (the daemon's single
// embedded bbolt database — see cmd/cerberusd/main.go) as a durable MetaStore.
// It does not own the store's lifecycle; the caller opens/closes it.
func NewBoltMetaStore(s *store.Store) *BoltMetaStore {
	return &BoltMetaStore{s: s}
}

// metaKey namespaces a /cer/fs path within metaBucket. Paths are already
// absolute ("/cer/fs/...") and therefore already unique per bucket, so no
// additional prefixing is needed beyond the bucket itself.
func metaKey(path string) string { return path }

// Put durably records the manifest for path (overwriting any prior file at that
// path). Returns once the write has been committed to the store.
func (b *BoltMetaStore) Put(path string, man dfs.Manifest) error {
	v, err := json.Marshal(man)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.s.Put(metaBucket, metaKey(path), v)
}

// Get returns the manifest for path, or false if no file was written there (or
// the stored record is unreadable, which is treated identically to "absent" so a
// read cannot fabricate bytes from a corrupt record).
func (b *BoltMetaStore) Get(path string) (dfs.Manifest, bool) {
	b.mu.Lock()
	v, ok, err := b.s.Get(metaBucket, metaKey(path))
	b.mu.Unlock()
	if err != nil || !ok {
		return dfs.Manifest{}, false
	}
	var man dfs.Manifest
	if jsonErr := json.Unmarshal(v, &man); jsonErr != nil {
		return dfs.Manifest{}, false
	}
	return man, true
}

// List returns every stored /cer/fs path in deterministic (sorted) order,
// reading the metadata bucket's keys directly from the store.
func (b *BoltMetaStore) List() ([]string, error) {
	b.mu.Lock()
	keys, err := b.s.Keys(metaBucket)
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

var _ MetaStore = (*BoltMetaStore)(nil)
