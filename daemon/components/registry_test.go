package components

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hash066/cerberus/daemon/store"
)

var helloShardWASM = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x0f, 0x01, 0x0b, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x5f, 0x73, 0x68, 0x61, 0x72, 0x64, 0x00, 0x00,
	0x0a, 0x07, 0x01, 0x05, 0x00, 0x41, 0xb9, 0x0a, 0x0b,
}

// TestAddListRestartList round-trips: add a component, list it, close and
// reopen the underlying bbolt store (simulating a daemon restart), then list
// again — the entry must still be there with the same CID.
func TestAddListRestartList(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "registry.db")
	wasmPath := filepath.Join(dir, "hello-shard.wasm")
	if err := os.WriteFile(wasmPath, helloShardWASM, 0o644); err != nil {
		t.Fatalf("write wasm fixture: %v", err)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	reg := Open(s)

	added, err := reg.Add("hello-shard", wasmPath)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if added.Name != "hello-shard" {
		t.Fatalf("name = %q, want hello-shard", added.Name)
	}
	if added.CID == "" {
		t.Fatal("added entry has no CID")
	}
	if added.Size != int64(len(helloShardWASM)) {
		t.Fatalf("size = %d, want %d", added.Size, len(helloShardWASM))
	}

	list, err := reg.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Name != "hello-shard" {
		t.Fatalf("list = %+v, want one hello-shard entry", list)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen — simulating a daemon restart. The entry must persist.
	s2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer s2.Close()
	reg2 := Open(s2)

	list2, err := reg2.List()
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	if len(list2) != 1 {
		t.Fatalf("list after reopen = %+v, want 1 entry", list2)
	}
	if list2[0].CID != added.CID {
		t.Fatalf("CID after reopen = %q, want %q", list2[0].CID, added.CID)
	}
	if list2[0].Path != added.Path {
		t.Fatalf("path after reopen = %q, want %q", list2[0].Path, added.Path)
	}

	got, err := reg2.Get("hello-shard")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.CID != added.CID {
		t.Fatalf("get CID after reopen = %q, want %q", got.CID, added.CID)
	}
}

// TestGetMissing proves a name that was never added yields ErrNotFound (not a
// panic, not an empty-but-nil-error result).
func TestGetMissing(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	reg := Open(s)

	if _, err := reg.Get("does-not-exist"); err == nil {
		t.Fatal("expected an error for an unregistered name")
	}
}

// TestBytesVerifiesIntegrity proves Bytes re-hashes the file at add time and
// rejects it if the on-disk file has since changed (no longer matches the
// recorded CID) — the same integrity discipline as wasm.ContentStore.Get.
func TestBytesVerifiesIntegrity(t *testing.T) {
	dir := t.TempDir()
	wasmPath := filepath.Join(dir, "hello-shard.wasm")
	if err := os.WriteFile(wasmPath, helloShardWASM, 0o644); err != nil {
		t.Fatalf("write wasm fixture: %v", err)
	}
	s, err := store.Open(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	reg := Open(s)

	if _, err := reg.Add("hello-shard", wasmPath); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Happy path: bytes still match.
	b, c, err := reg.Bytes("hello-shard")
	if err != nil {
		t.Fatalf("bytes: %v", err)
	}
	if string(b) != string(helloShardWASM) {
		t.Fatal("bytes mismatch")
	}
	if !c.Defined() {
		t.Fatal("returned CID is undefined")
	}

	// Corrupt the on-disk file after registration.
	if err := os.WriteFile(wasmPath, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, _, err := reg.Bytes("hello-shard"); err == nil {
		t.Fatal("Bytes did not detect that the on-disk file no longer matches the recorded CID")
	}
}

// TestAddOverwritesPreviousEntry proves re-adding the same name updates its
// entry rather than erroring or duplicating.
func TestAddOverwritesPreviousEntry(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	reg := Open(s)

	path1 := filepath.Join(dir, "a.wasm")
	path2 := filepath.Join(dir, "b.wasm")
	if err := os.WriteFile(path1, helloShardWASM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path2, append(append([]byte(nil), helloShardWASM...), 0x00), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := reg.Add("comp", path1)
	if err != nil {
		t.Fatalf("add 1: %v", err)
	}
	second, err := reg.Add("comp", path2)
	if err != nil {
		t.Fatalf("add 2: %v", err)
	}
	if first.CID == second.CID {
		t.Fatal("expected different CIDs for different content")
	}

	list, err := reg.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 entry after overwrite, got %d: %+v", len(list), list)
	}
	if list[0].CID != second.CID {
		t.Fatalf("expected the overwritten (second) CID to win, got %q want %q", list[0].CID, second.CID)
	}
}
