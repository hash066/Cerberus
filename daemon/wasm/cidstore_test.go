package wasm

import (
	"testing"

	"github.com/ipfs/go-cid"
)

func TestContentStorePutGetRoundTrip(t *testing.T) {
	s := NewContentStore()
	component := []byte{0x00, 0x61, 0x73, 0x6d, 1, 2, 3, 4}
	c, err := s.Put(component)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !s.Has(c) {
		t.Fatal("Has reported false for a stored CID")
	}
	got, err := s.Get(c)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(component) {
		t.Fatalf("got %x want %x", got, component)
	}
}

func TestComponentCIDDeterministicAndContentSensitive(t *testing.T) {
	a, err := ComponentCID([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	a2, err := ComponentCID([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Equals(a2) {
		t.Fatal("CID not deterministic for identical bytes")
	}
	b, err := ComponentCID([]byte("hellp"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Equals(b) {
		t.Fatal("different bytes produced the same CID")
	}
}

func TestContentStoreGetMissing(t *testing.T) {
	s := NewContentStore()
	c, _ := ComponentCID([]byte("absent"))
	if _, err := s.Get(c); err == nil {
		t.Fatal("expected error for missing CID")
	}
}

// TestContentStoreIntegrityCheck proves the worker-side integrity guarantee: a
// CID whose stored bytes do not hash back to it is rejected rather than served.
// We force the mismatch by stashing bytes under a foreign CID's key.
func TestContentStoreIntegrityCheck(t *testing.T) {
	s := NewContentStore()
	good := []byte("good-bytes")
	goodCID, _ := ComponentCID(good)

	// A CID for entirely different content.
	otherCID, _ := ComponentCID([]byte("other-content"))

	// Corrupt the store: map otherCID -> good bytes (bytes do not hash to it).
	s.mu.Lock()
	s.blocks[otherCID.KeyString()] = good
	s.mu.Unlock()

	if _, err := s.Get(otherCID); err == nil {
		t.Fatal("integrity check did not reject mismatched bytes")
	}
	// The correctly-stored CID still resolves.
	if _, err := s.Put(good); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(goodCID); err != nil {
		t.Fatalf("correctly stored CID failed to resolve: %v", err)
	}
}

func TestComponentCIDIsRawCodec(t *testing.T) {
	c, err := ComponentCID([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Type() != cid.Raw {
		t.Fatalf("codec = %d, want raw (%d)", c.Type(), cid.Raw)
	}
	if c.Version() != 1 {
		t.Fatalf("version = %d, want 1", c.Version())
	}
}
