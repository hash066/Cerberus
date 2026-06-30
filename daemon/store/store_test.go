package store

import (
	"path/filepath"
	"testing"
)

func TestPutGetDeleteAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("caps", "k1", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("caps", "k2", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen — data must survive (durability).
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	v, ok, err := s2.Get("caps", "k1")
	if err != nil || !ok || string(v) != "v1" {
		t.Fatalf("expected v1 after reopen, got %q ok=%v err=%v", v, ok, err)
	}

	keys, err := s2.Keys("caps")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}

	if err := s2.Delete("caps", "k1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s2.Get("caps", "k1"); ok {
		t.Fatal("k1 should be deleted")
	}
}

func TestMissingBucketAndKey(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, ok, err := s.Get("nope", "x"); err != nil || ok {
		t.Fatalf("missing bucket should be (nil,false,nil), got ok=%v err=%v", ok, err)
	}
	keys, err := s.Keys("nope")
	if err != nil || len(keys) != 0 {
		t.Fatalf("missing bucket Keys should be empty, got %v err=%v", keys, err)
	}
}
