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
	"fmt"
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
//
// The recover() calls in this file are defense in depth only, against a
// Go-side panic in this marshaling code (e.g. a slice-index or pointer-cast
// mistake). They cannot and do not catch a panic that originates inside the
// Rust cerberus_* call itself — that would unwind across the extern "C"
// boundary as undefined behavior before any Go defer runs. That hazard is
// closed on the Rust side by core/cabi/src/lib.rs's catch_unwind guard()
// wrapper around every exported fn.
func (Blockstore) Put(data []byte) (cid CID, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			cid, err = CID{}, fmt.Errorf("ffi: blockstore put panicked: %v", rec)
		}
	}()
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
func (Blockstore) Get(cid CID) (out []byte, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			out, err = nil, fmt.Errorf("ffi: blockstore get panicked: %v", rec)
		}
	}()
	cidPtr := (*C.uint8_t)(unsafe.Pointer(&cid[0]))
	n := C.cerberus_blockstore_get_len(cidPtr)
	if n < 0 {
		return nil, ErrNotFound
	}
	if n == 0 {
		return []byte{}, nil
	}
	out = make([]byte, int(n))
	written := C.cerberus_blockstore_get(cidPtr, (*C.uint8_t)(unsafe.Pointer(&out[0])), C.size_t(len(out)))
	if written < 0 {
		return nil, ErrNotFound
	}
	return out[:int(written)], nil
}

// Has reports whether the store holds a block for cid (without copying it).
// A recovered panic reports false (absent) rather than crashing the daemon;
// callers that need to distinguish "absent" from "internal error" should
// prefer Get, which returns an error.
func (Blockstore) Has(cid CID) (present bool) {
	defer func() {
		if recover() != nil {
			present = false
		}
	}()
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
// no-op, and there is no inverse. Any Go-side panic in the marshaling here is
// recovered and silently dropped (this method has no error return to report
// it through — matching the underlying cerberus_revoke ABI, which itself has
// no failure mode for a well-formed 16-byte id); see the file-level note above
// Blockstore.Put for what recover() can and cannot catch here.
func (RevocationSet) Revoke(id contract.CapID) {
	defer func() { _ = recover() }()
	C.cerberus_revoke((*C.uint8_t)(unsafe.Pointer(&id[0])))
}

// IsRevoked reports whether id is in the (merged) revocation set. A recovered
// panic fails closed (reports revoked/true), mirroring the Rust ABI's own
// convention (cerberus_is_revoked: "1 = revoked, 0 = live").
func (RevocationSet) IsRevoked(id contract.CapID) (revoked bool) {
	defer func() {
		if recover() != nil {
			revoked = true
		}
	}()
	return C.cerberus_is_revoked((*C.uint8_t)(unsafe.Pointer(&id[0]))) != 0
}

// Export serializes the local set as a concatenation of 16-byte cap ids in
// deterministic order. The wire format is defined by the cabi ABI (core/crdt
// exposes no serializer of its own — see the note in core/cabi/src/lib.rs); it
// is exactly what Merge consumes.
func (RevocationSet) Export() (out []byte, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			out, err = nil, fmt.Errorf("ffi: revocations export panicked: %v", rec)
		}
	}()
	n := C.cerberus_revocations_export_len()
	if n == 0 {
		return []byte{}, nil
	}
	out = make([]byte, int(n))
	written := C.cerberus_revocations_export((*C.uint8_t)(unsafe.Pointer(&out[0])), C.size_t(len(out)))
	if written < 0 {
		return nil, errors.New("ffi: revocations export failed")
	}
	return out[:int(written)], nil
}

// Merge folds another replica's exported set (from Export) into the local set,
// returning the number of ids merged. The merge is a set union: convergent,
// commutative, and idempotent, so replicas converge under any reorder/partition.
func (RevocationSet) Merge(data []byte) (n int, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			n, err = 0, fmt.Errorf("ffi: revocations merge panicked: %v", rec)
		}
	}()
	if len(data) == 0 {
		return 0, nil
	}
	merged := C.cerberus_revocations_merge((*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)))
	if merged < 0 {
		return 0, errors.New("ffi: revocations merge rejected malformed input")
	}
	return int(merged), nil
}
