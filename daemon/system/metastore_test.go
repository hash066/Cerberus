package system

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/hash066/cerberus/daemon/dfs"
	"github.com/hash066/cerberus/daemon/store"
)

// TestBoltMetaStoreSurvivesRestart proves the durable /cer/fs metadata store
// actually persists: write a Manifest for a path, close the store, reopen it
// against the SAME on-disk file (simulating a daemon restart), and confirm the
// path→Manifest mapping is still there — mirroring the settlement-durability
// test pattern in daemon/ledger/ledger_test.go (TestSettleWithChangeAndPersistence).
func TestBoltMetaStoreSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fsmeta.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	meta := NewBoltMetaStore(s)

	// Build a real Manifest the way dfs.Put would (contents don't matter here;
	// only that the whole struct round-trips through JSON exactly).
	fs, err := dfs.New(dfs.NewMemShardStore(), dfs.DefaultConfig())
	if err != nil {
		t.Fatalf("new dfs: %v", err)
	}
	data := bytes.Repeat([]byte("durable-metadata-"), 5000) // multi-chunk-ish payload
	man, err := fs.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("dfs put: %v", err)
	}

	const path1 = "/cer/fs/durable/example.bin"
	if err := meta.Put(path1, man); err != nil {
		t.Fatalf("meta put: %v", err)
	}

	// Sanity: readable before "restart".
	got, ok := meta.Get(path1)
	if !ok {
		t.Fatal("manifest not found immediately after Put")
	}
	if got.RootID != man.RootID {
		t.Fatalf("root id mismatch before restart: got %s want %s", got.RootID, man.RootID)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Reopen — simulating a daemon restart. The path→Manifest mapping must NOT be
	// lost; it must be loaded from the same durable store file.
	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer s2.Close()
	meta2 := NewBoltMetaStore(s2)

	got2, ok := meta2.Get(path1)
	if !ok {
		t.Fatal("manifest for path did not survive restart")
	}
	if got2.RootID != man.RootID {
		t.Fatalf("root id after restart = %s, want %s", got2.RootID, man.RootID)
	}
	if got2.TotalBytes != man.TotalBytes {
		t.Fatalf("total bytes after restart = %d, want %d", got2.TotalBytes, man.TotalBytes)
	}
	if len(got2.Chunks) != len(man.Chunks) {
		t.Fatalf("chunk count after restart = %d, want %d", len(got2.Chunks), len(man.Chunks))
	}

	// A path that was never written must still report "not found" after restart
	// (the store must not fabricate entries).
	if _, ok := meta2.Get("/cer/fs/never/written.bin"); ok {
		t.Fatal("BoltMetaStore reported a manifest for a path that was never written")
	}
}

// TestBoltMetaStoreOverwritesPath proves a second Put to the same path replaces
// the manifest (the durable store does not accumulate stale versions the reader
// could accidentally observe).
func TestBoltMetaStoreOverwritesPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fsmeta.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	meta := NewBoltMetaStore(s)

	fsEngine, err := dfs.New(dfs.NewMemShardStore(), dfs.DefaultConfig())
	if err != nil {
		t.Fatalf("new dfs: %v", err)
	}
	man1, err := fsEngine.Put(bytes.NewReader([]byte("version one")))
	if err != nil {
		t.Fatalf("put v1: %v", err)
	}
	man2, err := fsEngine.Put(bytes.NewReader([]byte("version two, a different length entirely")))
	if err != nil {
		t.Fatalf("put v2: %v", err)
	}

	const p = "/cer/fs/overwrite.bin"
	if err := meta.Put(p, man1); err != nil {
		t.Fatalf("put v1 meta: %v", err)
	}
	if err := meta.Put(p, man2); err != nil {
		t.Fatalf("put v2 meta: %v", err)
	}

	got, ok := meta.Get(p)
	if !ok {
		t.Fatal("manifest not found")
	}
	if got.RootID != man2.RootID {
		t.Fatalf("got root id %s, want the SECOND write's root id %s", got.RootID, man2.RootID)
	}
}
