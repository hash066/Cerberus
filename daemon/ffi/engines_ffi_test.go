//go:build ffi

package ffi

import (
	"bytes"
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
)

// These tests run only under `-tags ffi` and exercise the real Rust engines
// across the cgo boundary: the content-addressed BlockStore (core/runtime) and
// the sys/revocations OR-set (core/crdt).

func TestBlockstorePutGetRoundTrip(t *testing.T) {
	bs := NewBlockstore()
	payload := []byte("\x00asm\x01\x00\x00\x00 cgo round-trip component bytes")

	cid, err := bs.Put(payload)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !bs.Has(cid) {
		t.Fatal("store should report the freshly put CID present")
	}

	got, err := bs.Get(cid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("get returned %q, want %q", got, payload)
	}

	// Idempotent: identical bytes yield the identical CID.
	cid2, err := bs.Put(payload)
	if err != nil {
		t.Fatalf("put (2): %v", err)
	}
	if cid != cid2 {
		t.Fatal("identical bytes must produce identical CIDs")
	}
}

func TestBlockstoreTamperRejected(t *testing.T) {
	bs := NewBlockstore()
	cid, err := bs.Put([]byte("the genuine block"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	// Flip a digest byte: the CID now names content the store does not hold, so
	// the integrity re-check (Rust-side) must reject the fetch.
	tampered := cid
	tampered[cidLen-1] ^= 0xFF
	if bs.Has(tampered) {
		t.Fatal("store must not claim to hold a tampered CID")
	}
	if _, err := bs.Get(tampered); err != ErrNotFound {
		t.Fatalf("get(tampered) = %v, want ErrNotFound", err)
	}
}

func TestRevokeThenIsRevoked(t *testing.T) {
	rs := NewRevocationSet()
	var id contract.CapID
	for i := range id {
		id[i] = 0x5A
	}
	var other contract.CapID
	for i := range other {
		other[i] = 0x11
	}

	if rs.IsRevoked(id) {
		t.Fatal("fresh cap id must not be revoked")
	}
	rs.Revoke(id)
	if !rs.IsRevoked(id) {
		t.Fatal("revoked cap id must report revoked")
	}
	if rs.IsRevoked(other) {
		t.Fatal("an unrelated cap id must remain live")
	}
}

func TestRevocationsTwoSetMergeConverges(t *testing.T) {
	// The process holds a single RevocationSet singleton, so we model a "peer
	// replica" by revoking ids, exporting that state, and merging it back in —
	// the same bytes a remote node would gossip over the wire.
	rs := NewRevocationSet()

	var peerA, peerB contract.CapID
	for i := range peerA {
		peerA[i] = 0xA1
		peerB[i] = 0xB2
	}
	rs.Revoke(peerA)
	rs.Revoke(peerB)

	wire, err := rs.Export()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(wire) < 2*16 || len(wire)%16 != 0 {
		t.Fatalf("export produced %d bytes, want a positive multiple of 16", len(wire))
	}

	// Merge the serialized peer state back in: a no-op in value (already present)
	// but it must succeed and keep the revocations sticky (idempotent union).
	n, err := rs.Merge(wire)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if n != len(wire)/16 {
		t.Fatalf("merge reported %d ids, want %d", n, len(wire)/16)
	}
	if !rs.IsRevoked(peerA) || !rs.IsRevoked(peerB) {
		t.Fatal("revocations must survive a merge (convergence + stickiness)")
	}

	// Merging a brand-new id (a second peer that revoked something we hadn't
	// seen) must add it: this is the cross-replica propagation property.
	var peerC contract.CapID
	for i := range peerC {
		peerC[i] = 0xC3
	}
	if rs.IsRevoked(peerC) {
		t.Fatal("peerC should not be revoked before its merge")
	}
	if _, err := rs.Merge(peerC[:]); err != nil {
		t.Fatalf("merge peerC: %v", err)
	}
	if !rs.IsRevoked(peerC) {
		t.Fatal("a revoke on a peer must propagate via merge")
	}
}

func TestRevocationsMergeRejectsMisaligned(t *testing.T) {
	rs := NewRevocationSet()
	// 19 bytes is not a whole number of 16-byte cap ids.
	if _, err := rs.Merge(make([]byte, 19)); err == nil {
		t.Fatal("merge must reject a buffer that is not a multiple of 16 bytes")
	}
	// Empty input is an explicit no-op.
	if n, err := rs.Merge(nil); err != nil || n != 0 {
		t.Fatalf("merge(nil) = (%d, %v), want (0, nil)", n, err)
	}
}
