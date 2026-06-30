package dfs

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/reedsolomon"
)

// newFS builds an FS over a fresh in-memory store with the given config and
// returns both so tests can manipulate placement (drop/corrupt shards).
func newFS(t *testing.T, cfg Config) (*FS, *MemShardStore) {
	t.Helper()
	store := NewMemShardStore()
	fs, err := New(store, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return fs, store
}

func putGet(t *testing.T, fs *FS, data []byte) []byte {
	t.Helper()
	man, err := fs.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := fs.Get(man)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return got
}

// TestPutGetRoundTrip proves Put→Get returns the exact bytes, for an empty file,
// a sub-chunk file, an exact-chunk-boundary file, and a multi-chunk file.
func TestPutGetRoundTrip(t *testing.T) {
	cfg := Config{ChunkSize: 1024, DataShards: 4, ParityShards: 2}
	cases := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"tiny", 7},
		{"sub-chunk", 1000},
		{"exact-chunk", 1024},
		{"multi-chunk", 1024*3 + 137}, // spans 4 chunks, last partial
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, _ := newFS(t, cfg)
			data := make([]byte, tc.size)
			if _, err := rand.Read(data); err != nil {
				t.Fatalf("rand: %v", err)
			}
			got := putGet(t, fs, data)
			if !bytes.Equal(got, data) {
				t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(data))
			}
		})
	}
}

// TestManifestShape sanity-checks the manifest a multi-chunk Put produces.
func TestManifestShape(t *testing.T) {
	cfg := Config{ChunkSize: 1024, DataShards: 4, ParityShards: 2}
	fs, _ := newFS(t, cfg)
	data := make([]byte, 1024*2+1) // 3 chunks
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	man, err := fs.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if man.TotalBytes != int64(len(data)) {
		t.Fatalf("TotalBytes = %d, want %d", man.TotalBytes, len(data))
	}
	if len(man.Chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(man.Chunks))
	}
	if !man.RootID.Defined() {
		t.Fatal("RootID undefined")
	}
	for i, cm := range man.Chunks {
		if len(cm.ShardCIDs) != cfg.DataShards+cfg.ParityShards {
			t.Fatalf("chunk %d: %d shards, want %d", i, len(cm.ShardCIDs), cfg.DataShards+cfg.ParityShards)
		}
		if len(cm.Placement) != len(cm.ShardCIDs) {
			t.Fatalf("chunk %d: placement length mismatch", i)
		}
	}

	// Determinism: the same bytes produce the same root id.
	man2, err := fs.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put #2: %v", err)
	}
	if !man.RootID.Equals(man2.RootID) {
		t.Fatalf("RootID not deterministic: %s vs %s", man.RootID, man2.RootID)
	}
}

// TestReconstructAfterDroppingAnyMShards proves the file survives the loss of
// any m shards per chunk: for every combination of m dropped shard positions we
// drop those shards from the store and Get still returns the exact bytes.
func TestReconstructAfterDroppingAnyMShards(t *testing.T) {
	cfg := Config{ChunkSize: 1024, DataShards: 4, ParityShards: 2}
	total := cfg.DataShards + cfg.ParityShards // 6 shards, m=2 droppable

	data := make([]byte, 1024*2+200) // 3 chunks so each combo hits all chunks
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}

	// Every pair of shard positions to drop (m=2 losses).
	for a := 0; a < total; a++ {
		for b := a + 1; b < total; b++ {
			fs, store := newFS(t, cfg)
			man, err := fs.Put(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			for _, cm := range man.Chunks {
				store.Drop(cm.ShardCIDs[a])
				store.Drop(cm.ShardCIDs[b])
			}
			rc, err := fs.Get(man)
			if err != nil {
				t.Fatalf("drop {%d,%d}: Get: %v", a, b, err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close()
			if !bytes.Equal(got, data) {
				t.Fatalf("drop {%d,%d}: reconstruction mismatch", a, b)
			}
		}
	}
}

// TestFailsPastM proves that losing m+1 shards in a chunk is unrecoverable: Get
// must return an error (ErrTooFewShards), not silently corrupt output.
func TestFailsPastM(t *testing.T) {
	cfg := Config{ChunkSize: 1024, DataShards: 4, ParityShards: 2}
	fs, store := newFS(t, cfg)

	data := make([]byte, 1500) // 2 chunks
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	man, err := fs.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Drop m+1 = 3 shards from the first chunk.
	for i := 0; i < cfg.ParityShards+1; i++ {
		store.Drop(man.Chunks[0].ShardCIDs[i])
	}
	_, err = fs.Get(man)
	if err == nil {
		t.Fatal("expected Get to fail after losing m+1 shards, got nil")
	}
	if !errors.Is(err, reedsolomon.ErrTooFewShards) {
		t.Fatalf("expected ErrTooFewShards, got %v", err)
	}
}

// TestCorruptedShardRejectedByCID proves a tampered shard is caught by its CID
// before it can corrupt output. We corrupt one shard's bytes in place (more than
// m would otherwise be needed to recover, but here only 1 is bad so the chunk is
// still recoverable from the other 5): the corrupted shard is discarded on the
// CID check and the chunk is reconstructed cleanly, yielding the exact bytes.
func TestCorruptedShardRejectedByCID(t *testing.T) {
	cfg := Config{ChunkSize: 1024, DataShards: 4, ParityShards: 2}
	fs, store := newFS(t, cfg)

	data := make([]byte, 1024+300) // 2 chunks
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	man, err := fs.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Tamper with a data shard of the first chunk: same CID key, different bytes.
	target := man.Chunks[0].ShardCIDs[0]
	bad, err := store.GetShard(target)
	if err != nil {
		t.Fatalf("GetShard: %v", err)
	}
	bad[0] ^= 0xFF // flip a byte so it no longer hashes to its CID
	store.Corrupt(target, bad)

	rc, err := fs.Get(man)
	if err != nil {
		t.Fatalf("Get with one corrupt shard should still recover: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("corrupted shard leaked into output: CID integrity check failed to reject it")
	}
}

// TestCorruptionPlusLossIsCaught proves the CID check is load-bearing: if a shard
// is corrupted AND m other shards are lost, the chunk has only k-1 trustworthy
// shards, so Get must fail rather than feed the bad shard into Reconstruct and
// emit garbage.
func TestCorruptionPlusLossIsCaught(t *testing.T) {
	cfg := Config{ChunkSize: 1024, DataShards: 4, ParityShards: 2}
	fs, store := newFS(t, cfg)

	data := make([]byte, 900) // 1 chunk
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	man, err := fs.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	c := man.Chunks[0]
	// Lose m=2 shards and corrupt a third → only k-1=3 valid remain (<k).
	store.Drop(c.ShardCIDs[4])
	store.Drop(c.ShardCIDs[5])
	bad, _ := store.GetShard(c.ShardCIDs[0])
	bad[0] ^= 0xFF
	store.Corrupt(c.ShardCIDs[0], bad)

	if _, err := fs.Get(man); err == nil {
		t.Fatal("expected Get to fail when a corrupt shard leaves fewer than k valid shards")
	}
}

// TestNewValidation covers the constructor guards.
func TestNewValidation(t *testing.T) {
	if _, err := New(nil, DefaultConfig()); err == nil {
		t.Fatal("expected error for nil store")
	}
	if _, err := New(NewMemShardStore(), Config{}); err == nil {
		t.Fatal("expected error for zero config")
	}
}
