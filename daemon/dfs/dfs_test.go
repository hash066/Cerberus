package dfs

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/ipfs/go-cid"
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

// --- random access (File) ---------------------------------------------------

// countingStore wraps a MemShardStore and counts shard fetches, so a test can
// prove a ranged read touches only the chunks it needs rather than the file.
type countingStore struct {
	*MemShardStore
	gets int
}

func (c *countingStore) GetShard(id cid.Cid) ([]byte, error) {
	c.gets++
	return c.MemShardStore.GetShard(id)
}

// TestFileReadAtMatchesGet proves the random-access reader returns exactly the
// bytes Get does, at every offset — across chunk boundaries, for reads longer
// than a chunk, and at EOF.
func TestFileReadAtMatchesGet(t *testing.T) {
	cfg := Config{ChunkSize: 64, DataShards: 4, ParityShards: 2}
	fs, _ := newFS(t, cfg)
	data := make([]byte, 64*5+7) // 6 chunks, last one short
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	man, err := fs.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	f, err := fs.Open(man)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	if f.Size() != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", f.Size(), len(data))
	}
	// io.ReaderAt semantics are exactly what a mount's read(2) relies on, so
	// verify against the stdlib's own conformance expectations at many offsets.
	for _, n := range []int{1, 7, 64, 65, 200, len(data)} {
		for _, off := range []int64{0, 1, 63, 64, 65, 127, int64(len(data)) - 1} {
			buf := make([]byte, n)
			got, err := f.ReadAt(buf, off)
			want := data[off:]
			if len(want) > n {
				want = want[:n]
			}
			if got != len(want) {
				t.Fatalf("ReadAt(%d @ %d) read %d bytes, want %d", n, off, got, len(want))
			}
			if !bytes.Equal(buf[:got], want) {
				t.Fatalf("ReadAt(%d @ %d) returned the wrong bytes", n, off)
			}
			// Short reads must always carry a non-nil error (io.ReaderAt contract).
			if got < n && err == nil {
				t.Fatalf("ReadAt(%d @ %d) short read reported no error", n, off)
			}
		}
	}
	if _, err := f.ReadAt(make([]byte, 4), int64(len(data))); err != io.EOF {
		t.Fatalf("read at EOF should be io.EOF, got %v", err)
	}
	if _, err := f.ReadAt(make([]byte, 4), -1); err == nil {
		t.Fatal("read at a negative offset must fail")
	}
}

// TestFileReadAtIsChunkRangedNotWholeFile is the reason File exists: a small
// read must reconstruct ONLY the chunk it lands in. If a read of 8 bytes pulled
// every shard of every chunk, serving a 4 KiB read of a 4 GiB file through a
// mount would cost the whole 4 GiB — which is exactly the design being avoided.
func TestFileReadAtIsChunkRangedNotWholeFile(t *testing.T) {
	cfg := Config{ChunkSize: 64, DataShards: 4, ParityShards: 2}
	store := &countingStore{MemShardStore: NewMemShardStore()}
	fs, err := New(store, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	data := make([]byte, 64*20) // 20 chunks
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	man, err := fs.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	f, err := fs.Open(man)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	perChunk := cfg.DataShards + cfg.ParityShards
	store.gets = 0
	buf := make([]byte, 8)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if store.gets > perChunk {
		t.Fatalf("an 8-byte read fetched %d shards; one chunk is %d — the read is not chunk-ranged",
			store.gets, perChunk)
	}
	if !bytes.Equal(buf, data[:8]) {
		t.Fatal("ranged read returned the wrong bytes")
	}

	// A second read inside the SAME chunk must be served from the cache without
	// re-fetching a single shard: the kernel reads a file in small slices, so
	// without this a sequential scan would re-decode each chunk many times.
	store.gets = 0
	if _, err := f.ReadAt(buf, 16); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if store.gets != 0 {
		t.Fatalf("a second read within a cached chunk fetched %d shards, want 0", store.gets)
	}

	// Reading the LAST chunk must not walk the file to get there.
	store.gets = 0
	if _, err := f.ReadAt(buf, int64(len(data))-8); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if store.gets > perChunk {
		t.Fatalf("reading the last chunk fetched %d shards; one chunk is %d", store.gets, perChunk)
	}
}

// TestFileReadAtReconstructsFromParity proves the ranged read path keeps the
// integrity guarantees of Get: it reuses the same getChunk, so a lost shard is
// rebuilt from parity rather than silently returning wrong bytes.
func TestFileReadAtReconstructsFromParity(t *testing.T) {
	cfg := Config{ChunkSize: 64, DataShards: 4, ParityShards: 2}
	fs, store := newFS(t, cfg)
	data := make([]byte, 64*3)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	man, err := fs.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Drop m=2 shards from the first chunk: still recoverable.
	store.Drop(man.Chunks[0].ShardCIDs[0])
	store.Drop(man.Chunks[0].ShardCIDs[1])

	f, err := fs.Open(man)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 64)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt after losing m shards should reconstruct: %v", err)
	}
	if !bytes.Equal(buf, data[:64]) {
		t.Fatal("reconstructed ranged read returned the wrong bytes")
	}

	// Past m: the read must FAIL, never return wrong or zero-filled bytes.
	store.Drop(man.Chunks[1].ShardCIDs[0])
	store.Drop(man.Chunks[1].ShardCIDs[1])
	store.Drop(man.Chunks[1].ShardCIDs[2])
	f2, _ := fs.Open(man)
	defer f2.Close()
	if _, err := f2.ReadAt(buf, 64); err == nil {
		t.Fatal("a read of a chunk with fewer than k valid shards must fail, not fabricate bytes")
	}
}

// TestFileOpenRejectsMalformedManifest: random access maps an offset to a chunk
// arithmetically, which is only valid because every chunk but the last is full.
// A manifest violating that must be refused at Open, not mis-read at ReadAt.
func TestFileOpenRejectsMalformedManifest(t *testing.T) {
	cfg := Config{ChunkSize: 64, DataShards: 4, ParityShards: 2}
	fs, _ := newFS(t, cfg)
	man, err := fs.Put(bytes.NewReader(make([]byte, 64*3)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	bad := man
	bad.Chunks = append([]ChunkManifest(nil), man.Chunks...)
	bad.Chunks[0].ChunkBytes = 32 // a short chunk that is not the last one
	if _, err := fs.Open(bad); err == nil {
		t.Fatal("Open must reject a manifest whose non-final chunk is short")
	}

	if _, err := fs.Open(Manifest{DataShards: 0}); err == nil {
		t.Fatal("Open must reject a manifest with invalid shard counts")
	}
	if _, err := fs.Open(Manifest{DataShards: 4, ParityShards: 2, ChunkSize: 0}); err == nil {
		t.Fatal("Open must reject a manifest with no chunk size")
	}
}
