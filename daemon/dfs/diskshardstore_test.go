package dfs

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// TestDiskShardStoreSurvivesReopen is the regression this store exists for: a
// composed daemon paired DURABLE metadata (BoltMetaStore) with an IN-MEMORY shard
// store, so after a restart /cer/fs still listed every file and could read none of
// them — `fs get` failed with "too few shards given: have 0 valid shards, need 4".
// Reopening the store is what a daemon restart looks like to this layer.
func TestDiskShardStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("shard bytes that must outlive the process that wrote them")

	first, err := NewDiskShardStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c, err := ShardCID(payload)
	if err != nil {
		t.Fatalf("cid: %v", err)
	}
	if _, err := first.PutShard(c, payload); err != nil {
		t.Fatalf("put: %v", err)
	}

	// A brand-new store over the same directory — i.e. the daemon restarted.
	second, err := NewDiskShardStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := second.GetShard(c)
	if err != nil {
		t.Fatalf("shard did not survive a reopen — this is the exact bug: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("shard bytes changed across reopen: got %q want %q", got, payload)
	}
}

// TestDiskShardStoreRoundTripsThroughFS proves the store is usable by the real
// erasure-coding engine (not just as a byte map), and that a file written before a
// "restart" is readable after one — end to end, through Put/Get.
func TestDiskShardStoreRoundTripsThroughFS(t *testing.T) {
	dir := t.TempDir()
	blob := make([]byte, 3<<20) // 3 MiB: several chunks, so parity is real
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}

	store, err := NewDiskShardStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := New(store, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	man, err := fs.Put(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	// Restart: a fresh store and a fresh FS over the same directory. Only the
	// Manifest carries over, exactly as a daemon's durable metadata would.
	reopened, err := NewDiskShardStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	fs2, err := New(reopened, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	rc, err := fs2.Get(man)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	defer func() { _ = rc.Close() }()
	var out bytes.Buffer
	if _, err := out.ReadFrom(rc); err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if !bytes.Equal(out.Bytes(), blob) {
		t.Fatalf("content changed across reopen: got %d bytes, want %d", out.Len(), len(blob))
	}
}

// TestDiskShardStoreParityRebuildsAfterLoss proves the durable store keeps dfs's
// integrity story intact: losing shards on disk (a bad sector, a stray delete) is
// recovered from parity, and a MISS is reported as an error rather than as empty
// bytes — the distinction dfs.Get relies on to know a shard needs rebuilding.
func TestDiskShardStoreParityRebuildsAfterLoss(t *testing.T) {
	dir := t.TempDir()
	// Random, NOT repetitive: see TestShardDedupCollapsesRedundancy below for why
	// repetitive content makes every shard of a chunk share one CID — and so one
	// stored object — which would make this test fail for a reason that has
	// nothing to do with the store.
	blob := make([]byte, 1<<21) // 2 MiB
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}

	store, err := NewDiskShardStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := New(store, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	man, err := fs.Put(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}

	// Destroy one shard of the first chunk. DefaultConfig is k=4+m=2, so one
	// loss must be survivable.
	if len(man.Chunks) == 0 || len(man.Chunks[0].ShardCIDs) == 0 {
		t.Fatal("manifest has no shards to drop")
	}
	lost := man.Chunks[0].ShardCIDs[0]
	store.Drop(lost)
	if _, err := store.GetShard(lost); err == nil {
		t.Fatal("a dropped shard must report a miss, not succeed")
	}

	rc, err := fs.Get(man)
	if err != nil {
		t.Fatalf("parity did not cover a single lost shard: %v", err)
	}
	defer func() { _ = rc.Close() }()
	var out bytes.Buffer
	if _, err := out.ReadFrom(rc); err != nil {
		t.Fatalf("read after shard loss: %v", err)
	}
	if !bytes.Equal(out.Bytes(), blob) {
		t.Fatal("content differs after parity rebuild")
	}
}

// TestShardDedupCollapsesRedundancy documents a REAL, CURRENTLY-UNFIXED hazard
// in the interaction between erasure coding and content addressing. It is not a
// property of any particular ShardStore — it reproduces on MemShardStore too —
// and it is not what this file's store introduced. It is pinned here because it
// was found here, and because a silent version of it is dangerous.
//
// THE HAZARD: shards are addressed by the CID of their BYTES. When a chunk's
// content is repetitive (a run of zeros, a padded region, a repeated pattern —
// all common in real files), every data shard of that chunk is byte-identical,
// so they share ONE CID. The Reed-Solomon parity of identical data shards can
// equal that same value too. The result: all k+m shards of a chunk collapse to a
// SINGLE stored object.
//
// The Manifest still lists k+m shard CIDs, and dfs still reports k=4+m=2, so the
// file LOOKS like it survives two losses. It does not: there is one object, and
// losing it loses everything. The redundancy is arithmetic, not physical.
//
// This test asserts the CURRENT behaviour so the property is visible and any
// future change to it is deliberate. If dedup is later made shard-position-aware
// (e.g. by salting a shard's hash with its chunk+index, which would cost dedup
// but buy real independence), this test SHOULD fail and be updated.
func TestShardDedupCollapsesRedundancy(t *testing.T) {
	store := NewMemShardStore() // NOT the disk store: this is a dfs-level property
	fs, err := New(store, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	// Repetitive content: the pattern length divides the shard length, so every
	// data shard of a chunk is byte-identical.
	blob := bytes.Repeat([]byte("cerberus"), 200_000)
	man, err := fs.Put(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}

	uniq := map[string]struct{}{}
	for _, c := range man.Chunks[0].ShardCIDs {
		uniq[c.String()] = struct{}{}
	}
	total := len(man.Chunks[0].ShardCIDs)
	if len(uniq) == total {
		t.Skipf("shards of a repetitive chunk are no longer deduplicating "+
			"(%d unique of %d) — the hazard this test documents may have been "+
			"fixed; re-read the comment and update it", len(uniq), total)
	}
	t.Logf("HAZARD PRESENT (documented, unfixed): %d shards of a repetitive chunk "+
		"share %d unique CID(s) — the k+m redundancy is arithmetic, not physical",
		total, len(uniq))

	// Prove the consequence rather than merely asserting the CID count: dropping
	// the ONE object the shards share destroys the file, even though the manifest
	// still advertises k+m shards and dfs tolerates m losses.
	lost := man.Chunks[0].ShardCIDs[0]
	store.Drop(lost)
	if _, err := fs.Get(man); err == nil {
		t.Fatal("expected the file to be unrecoverable after dropping the single " +
			"object every shard deduplicated to — if this now passes, dedup no " +
			"longer collapses redundancy and this test needs rewriting")
	}
}

// TestDiskShardStoreLeavesNoTempFiles pins the atomic-write contract: a committed
// shard leaves exactly one file and no ".shard-*" temp litter. A truncated file at
// a shard's real path would hash wrong, so dfs would treat it as corrupt and burn
// parity that exists to survive an actual disk failure.
func TestDiskShardStoreLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := NewDiskShardStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("atomic")
	c, err := ShardCID(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutShard(c, payload); err != nil {
		t.Fatal(err)
	}
	// Re-put the same CID: content addressing makes it a no-op, and it must not
	// litter either.
	if _, err := store.PutShard(c, payload); err != nil {
		t.Fatal(err)
	}

	var temps []string
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && len(d.Name()) >= 7 && d.Name()[:7] == ".shard-" {
			temps = append(temps, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temp files left behind: %v", temps)
	}
}
