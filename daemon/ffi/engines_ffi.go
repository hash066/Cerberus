//go:build ffi

// Real Go<->Rust bindings for the two engine surfaces added in Phase E:
//   - the content-addressed BlockStore (E2, core/runtime), and
//   - the sys/revocations OR-set (E4, core/crdt).
//
// Like kernel_ffi.go these are enabled only with `-tags ffi`, share the same
// cabi static lib (the #cgo directives live in kernel_ffi.go), and keep the seam
// tiny: bytes cross into caller-provided buffers, all engine state stays
// Rust-side behind process-wide singletons. The default (non-ffi) build never
// references these types, so no pure-Go stub is required.
package ffi

/*
#include <stdlib.h>
#include "cerberus.h"
*/
import "C"

import (
	"errors"
	"unsafe"

	contract "github.com/hash066/cerberus/contract/go"
)

// cidLen is the byte length of the self-describing CID binary form crossing the
// ABI (must match CER_CID_LEN in cerberus.h / Cid::to_bytes in core/runtime).
const cidLen = 36

// CID is the 36-byte content identifier returned by the block store. It is the
// canonical binary form (<v1><raw><sha2-256><len><digest>), opaque to Go.
type CID [cidLen]byte

// ErrNotFound is returned by Blockstore.Get when no block matches the CID, the
// CID is malformed, or the stored bytes fail the store's integrity re-check.
var ErrNotFound = errors.New("ffi: block not found or integrity check failed")

// Blockstore is a handle to the process-wide Rust BlockStore. Content is
// SHA-256-addressed and integrity-checked on Get (re-hash-on-get lives Rust-side),
// so a caller can never retrieve bytes that do not hash to the requested CID.
//
// The store is a process singleton in the Rust core; every Blockstore value
// refers to the same underlying store.
type Blockstore struct{}

// NewBlockstore returns a handle to the Rust content-addressed block store.
func NewBlockstore() Blockstore { return Blockstore{} }

// Put stores data and returns its content id. Idempotent: identical bytes yield
// the same CID without duplicating storage.
func (Blockstore) Put(data []byte) (CID, error) {
	var cid CID
	var dataPtr *C.uint8_t
	if len(data) > 0 {
		dataPtr = (*C.uint8_t)(unsafe.Pointer(&data[0]))
	}
	rc := C.cerberus_blockstore_put(dataPtr, C.size_t(len(data)), (*C.uint8_t)(unsafe.Pointer(&cid[0])))
	if rc != 0 {
		return CID{}, errors.New("ffi: blockstore put failed")
	}
	return cid, nil
}

// Get returns the bytes named by cid, re-verified to hash to it. It returns
// ErrNotFound if the block is absent, the CID is malformed, or integrity fails.
// Uses the ABI's size-then-copy pattern (ask the length, allocate, copy).
func (Blockstore) Get(cid CID) ([]byte, error) {
	cidPtr := (*C.uint8_t)(unsafe.Pointer(&cid[0]))
	n := C.cerberus_blockstore_get_len(cidPtr)
	if n < 0 {
		return nil, ErrNotFound
	}
	if n == 0 {
		return []byte{}, nil
	}
	out := make([]byte, int(n))
	written := C.cerberus_blockstore_get(cidPtr, (*C.uint8_t)(unsafe.Pointer(&out[0])), C.size_t(len(out)))
	if written < 0 {
		return nil, ErrNotFound
	}
	return out[:int(written)], nil
}

// Has reports whether the store holds a block for cid (without copying it).
func (Blockstore) Has(cid CID) bool {
	return C.cerberus_blockstore_has((*C.uint8_t)(unsafe.Pointer(&cid[0]))) != 0
}

// RevocationSet is a handle to the process-wide Rust sys/revocations OR-set
// (core/crdt RevocationSet): a grow-only, convergent set of revoked capability
// ids. Revoke adds; IsRevoked queries; Merge folds in another replica's set so a
// revoke on one node propagates to another (sticky, order-independent).
//
// The set is a process singleton in the Rust core; every RevocationSet value
// refers to the same underlying set.
type RevocationSet struct{}

// NewRevocationSet returns a handle to the Rust sys/revocations OR-set.
func NewRevocationSet() RevocationSet { return RevocationSet{} }

// Revoke adds id to the revocation set. Monotone: re-revoking the same id is a
// no-op, and there is no inverse.
func (RevocationSet) Revoke(id contract.CapID) {
	C.cerberus_revoke((*C.uint8_t)(unsafe.Pointer(&id[0])))
}

// IsRevoked reports whether id is in the (merged) revocation set.
func (RevocationSet) IsRevoked(id contract.CapID) bool {
	return C.cerberus_is_revoked((*C.uint8_t)(unsafe.Pointer(&id[0]))) != 0
}

// Export serializes the local set as a concatenation of 16-byte cap ids in
// deterministic order. The wire format is defined by the cabi ABI (core/crdt
// exposes no serializer of its own — see the note in core/cabi/src/lib.rs); it
// is exactly what Merge consumes.
func (RevocationSet) Export() ([]byte, error) {
	n := C.cerberus_revocations_export_len()
	if n == 0 {
		return []byte{}, nil
	}
	out := make([]byte, int(n))
	written := C.cerberus_revocations_export((*C.uint8_t)(unsafe.Pointer(&out[0])), C.size_t(len(out)))
	if written < 0 {
		return nil, errors.New("ffi: revocations export failed")
	}
	return out[:int(written)], nil
}

// Merge folds another replica's exported set (from Export) into the local set,
// returning the number of ids merged. The merge is a set union: convergent,
// commutative, and idempotent, so replicas converge under any reorder/partition.
func (RevocationSet) Merge(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	merged := C.cerberus_revocations_merge((*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)))
	if merged < 0 {
		return 0, errors.New("ffi: revocations merge rejected malformed input")
	}
	return int(merged), nil
}
