package wasm

// cidstore.go is the Go-side content-addressed component store. A WASM component
// is addressed by the SHA-256 CIDv1 (raw codec) of its bytes; a worker resolves
// a task's component CID against this store and verifies the resolved bytes hash
// back to the requested CID before executing. This is the real integrity check
// required by Vertical 03 §7 ("inputs/outputs are content-addressed (CID), so a
// worker cannot be fed an unverifiable payload"). It mirrors the SHA-256 CIDv1
// content store in core/runtime (Phase E2, Rust) on the Go control-plane side.
//
// The store itself is in-memory and local-only -- correct as a per-node cache,
// but not persistent across a restart. Peer-to-peer fetch on a local miss is
// real and lives one layer up: daemon/mesh/component.go's ServeComponentFetch/
// RequestComponent asks a mesh peer for the bytes (re-verifying the CID before
// accepting them) and populates this store on success. A persistent (on-disk)
// cache is the remaining documented next step; the peer-fetch half is not a
// stub anymore.

import (
	"fmt"
	"sync"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

// ComponentCID returns the canonical content address of a WASM component:
// CIDv1 with the raw codec over the SHA-256 multihash of the bytes. The same
// bytes always produce the same CID, and any change to the bytes changes the CID.
func ComponentCID(component []byte) (cid.Cid, error) {
	mh, err := multihash.Sum(component, multihash.SHA2_256, -1)
	if err != nil {
		return cid.Undef, fmt.Errorf("wasm: hash component: %w", err)
	}
	return cid.NewCidV1(cid.Raw, mh), nil
}

// ContentStore maps a component CID to the exact bytes that hash to it.
type ContentStore struct {
	mu     sync.RWMutex
	blocks map[string][]byte // key: cid.KeyString()
}

// NewContentStore returns an empty content store.
func NewContentStore() *ContentStore {
	return &ContentStore{blocks: map[string][]byte{}}
}

// Put stores a component and returns its CID. The stored bytes are copied so a
// later mutation of the caller's slice cannot break the content-addressing.
func (s *ContentStore) Put(component []byte) (cid.Cid, error) {
	c, err := ComponentCID(component)
	if err != nil {
		return cid.Undef, err
	}
	cp := append([]byte(nil), component...)
	s.mu.Lock()
	s.blocks[c.KeyString()] = cp
	s.mu.Unlock()
	return c, nil
}

// Get resolves a component CID to its bytes and verifies integrity: the resolved
// bytes are re-hashed and must produce exactly the requested CID. A mismatch
// (corruption or a substituted block) is reported rather than executed.
func (s *ContentStore) Get(c cid.Cid) ([]byte, error) {
	s.mu.RLock()
	b, ok := s.blocks[c.KeyString()]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("wasm: component %s not found in content store", c)
	}
	got, err := ComponentCID(b)
	if err != nil {
		return nil, err
	}
	if !got.Equals(c) {
		return nil, fmt.Errorf("wasm: content store integrity check failed: stored bytes hash to %s, not %s", got, c)
	}
	return append([]byte(nil), b...), nil
}

// Has reports whether a component CID is present.
func (s *ContentStore) Has(c cid.Cid) bool {
	s.mu.RLock()
	_, ok := s.blocks[c.KeyString()]
	s.mu.RUnlock()
	return ok
}
