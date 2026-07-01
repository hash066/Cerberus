// Package components is a small, local, durable name -> component registry,
// backed by daemon/store (bbolt) exactly like daemon/ledger. It answers one
// question: "what CID does the component I called <name> resolve to, and
// where can I find its bytes on this machine?"
//
// This is deliberately NOT a marketplace, a publishing system, or a
// mesh-synced registry. Each node's registry is its own local catalogue of
// names it finds convenient; nothing here talks to a peer. See CLAUDE.md and
// the task description this shipped under: "a local convenience for naming
// components you have," not a distributed/synced system. If you're tempted to
// add sync/replication here, that is explicitly out of scope for this package
// — see daemon/mesh's peer component-fetch path (component.go) for the actual
// p2p transport of component bytes, which is a separate, smaller concern.
package components

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hash066/cerberus/daemon/store"
	"github.com/hash066/cerberus/daemon/wasm"
	"github.com/ipfs/go-cid"
)

// bucket is the single bbolt bucket this registry lives in, keyed by
// component name (mirrors daemon/ledger's bucket-constant convention).
const bucket = "components"

// ErrNotFound is returned when a name has no registered component.
var ErrNotFound = errors.New("components: no such component")

// Entry is a named component: its content address and where its bytes live
// on disk. Path is a local convenience — v1 does not attempt to make it
// portable across machines.
type Entry struct {
	Name string `json:"name"`
	CID  string `json:"cid"`  // canonical string form of the CID
	Path string `json:"path"` // local filesystem path to the .wasm bytes
	Size int64  `json:"size"` // byte length of the component
}

// Registry is a durable name -> Entry catalogue, backed by bbolt.
type Registry struct {
	s *store.Store
}

// Open wraps an already-opened durable store. Callers own the store's
// lifecycle (Open/Close), exactly as daemon/ledger.Open does.
func Open(s *store.Store) *Registry {
	return &Registry{s: s}
}

// Add reads the WASM bytes at path, content-addresses them (the same
// SHA-256 CIDv1 raw-codec scheme daemon/wasm.ContentStore uses, so the CID
// this registry records is exactly what a worker will resolve a task
// against), and durably registers name -> Entry. Re-adding the same name
// overwrites its previous entry (the last add wins).
func (r *Registry) Add(name, path string) (Entry, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Entry{}, errors.New("components: name is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, fmt.Errorf("components: read %s: %w", path, err)
	}
	c, err := wasm.ComponentCID(b)
	if err != nil {
		return Entry{}, fmt.Errorf("components: hash %s: %w", path, err)
	}
	abs := path
	if a, aerr := filepath.Abs(path); aerr == nil {
		abs = a
	}
	e := Entry{Name: name, CID: c.String(), Path: abs, Size: int64(len(b))}
	v, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	if err := r.s.Put(bucket, name, v); err != nil {
		return Entry{}, fmt.Errorf("components: persist %s: %w", name, err)
	}
	return e, nil
}

// Get resolves a registered name to its Entry.
func (r *Registry) Get(name string) (Entry, error) {
	b, ok, err := r.s.Get(bucket, name)
	if err != nil {
		return Entry{}, err
	}
	if !ok {
		return Entry{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return Entry{}, fmt.Errorf("components: decode entry %q: %w", name, err)
	}
	return e, nil
}

// List returns every registered component, sorted by name.
func (r *Registry) List() ([]Entry, error) {
	keys, err := r.s.Keys(bucket)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(keys))
	for _, k := range keys {
		b, ok, err := r.s.Get(bucket, k)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var e Entry
		if err := json.Unmarshal(b, &e); err != nil {
			continue // skip a corrupt record rather than fail the whole list
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Bytes reads a registered component's bytes from its recorded path and
// verifies they still hash to the recorded CID before returning them — the
// same integrity discipline daemon/wasm.ContentStore.Get applies, so a
// registry entry can never be used to smuggle unverifiable bytes into a
// worker's content store.
func (r *Registry) Bytes(name string) ([]byte, cid.Cid, error) {
	e, err := r.Get(name)
	if err != nil {
		return nil, cid.Undef, err
	}
	b, err := os.ReadFile(e.Path)
	if err != nil {
		return nil, cid.Undef, fmt.Errorf("components: read %s: %w", e.Path, err)
	}
	want, err := cid.Decode(e.CID)
	if err != nil {
		return nil, cid.Undef, fmt.Errorf("components: bad recorded cid for %q: %w", name, err)
	}
	got, err := wasm.ComponentCID(b)
	if err != nil {
		return nil, cid.Undef, err
	}
	if !got.Equals(want) {
		return nil, cid.Undef, fmt.Errorf("components: %q integrity check failed: %s now hashes to %s, not recorded %s", name, e.Path, got, want)
	}
	return b, want, nil
}
