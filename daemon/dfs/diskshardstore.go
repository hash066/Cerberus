package dfs

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ipfs/go-cid"
)

// DiskShardStore is a durable, content-addressed ShardStore: one file per shard,
// named by its CID, under a root directory.
//
// WHY THIS EXISTS: the composed daemon paired a DURABLE metadata store
// (BoltMetaStore) with an IN-MEMORY MemShardStore. Metadata therefore survived a
// restart and the bytes did not, so /cer/fs would list a file it could no longer
// read — `fs get` failed with "too few shards given: have 0 valid shards, need 4".
// A filesystem that confidently lists data it has lost is worse than one that
// admits it stored nothing, which is why this is a correctness fix and not an
// optimization.
//
// Layout: <root>/<first 2 hex bytes of the CID's multihash digest>/<full CID>.shard
// The 256-way fan-out keeps directories small enough for a filesystem to stat
// efficiently once a node holds many shards (git and IPFS's blockstore both do
// this, for the same reason).
//
// Integrity is NOT re-checked here on read: dfs.getChunk already verifies every
// shard's bytes against its CID and reconstructs from parity on a mismatch (see
// Get). Re-hashing here would duplicate that work on the hot path while adding no
// authority — a corrupt shard is dropped and rebuilt either way. A miss and a
// corrupt shard are both simply "this shard is unavailable", exactly as the
// ShardStore contract requires.
type DiskShardStore struct {
	root string
}

// NewDiskShardStore opens (creating if needed) a durable shard store rooted at
// dir. Every shard written survives a daemon restart.
func NewDiskShardStore(dir string) (*DiskShardStore, error) {
	if dir == "" {
		return nil, errors.New("dfs: DiskShardStore needs a root directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("dfs: create shard store root %s: %w", dir, err)
	}
	return &DiskShardStore{root: dir}, nil
}

// Root reports the directory this store writes to.
func (d *DiskShardStore) Root() string { return d.root }

// pathFor maps a CID to its on-disk location. It uses the multihash DIGEST's
// leading bytes for the fan-out rather than the CID string, so the spread is over
// hash bytes (uniform) instead of the shared multibase/codec prefix every CID
// here starts with (which would put every shard in one directory).
func (d *DiskShardStore) pathFor(c cid.Cid) string {
	digest := c.Hash()
	prefix := "00"
	if len(digest) >= 2 {
		// Skip the multihash header (code + length) so the fan-out is over real
		// digest bytes; a short/odd hash falls back to the first byte available.
		start := 2
		if len(digest) < start+1 {
			start = 0
		}
		prefix = hex.EncodeToString(digest[start : start+1])
	}
	return filepath.Join(d.root, prefix, c.String()+".shard")
}

// PutShard durably writes the shard's bytes under its CID and reports the
// placement hint "disk".
//
// The write is atomic: bytes go to a temp file in the same directory and are
// renamed into place, so a crash mid-write can never leave a TRUNCATED file at a
// shard's path. That matters more than it looks — a short file at the right name
// would hash differently, so dfs would treat the shard as corrupt and rebuild it
// from parity. Correct, but it would silently burn the redundancy that exists to
// survive a real disk failure.
//
// Content addressing makes a rewrite a no-op by definition: the same CID always
// means the same bytes.
func (d *DiskShardStore) PutShard(c cid.Cid, shard []byte) (string, error) {
	p := d.pathFor(c)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", fmt.Errorf("dfs: shard dir: %w", err)
	}
	if _, err := os.Stat(p); err == nil {
		return "disk", nil // already stored; same CID means the same bytes
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".shard-*")
	if err != nil {
		return "", fmt.Errorf("dfs: temp shard: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed
	if _, err := tmp.Write(shard); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("dfs: write shard: %w", err)
	}
	// fsync before rename: without it the rename can land while the bytes are
	// still only in the page cache, which is exactly the crash window this
	// atomic-write dance exists to close.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("dfs: sync shard: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("dfs: close shard: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return "", fmt.Errorf("dfs: commit shard: %w", err)
	}
	return "disk", nil
}

// GetShard reads a shard's bytes back. A miss returns an error, which dfs treats
// as "unavailable" and recovers from via parity (as long as k shards remain).
func (d *DiskShardStore) GetShard(c cid.Cid) ([]byte, error) {
	b, err := os.ReadFile(d.pathFor(c))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("dfs: shard %s not found", c)
		}
		return nil, fmt.Errorf("dfs: read shard %s: %w", c, err)
	}
	return b, nil
}

// Drop removes a shard's bytes. Not part of the ShardStore interface; it mirrors
// MemShardStore.Drop so tests can simulate real shard loss against the durable
// store too.
func (d *DiskShardStore) Drop(c cid.Cid) {
	_ = os.Remove(d.pathFor(c))
}

// compile-time assertion that the durable store satisfies the same contract the
// in-memory one does.
var _ ShardStore = (*DiskShardStore)(nil)
