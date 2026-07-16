// Package dfs is Phase G2 — the distributed filesystem behind /cer/fs
// (vertical 04 §3, ARCHITECTURE.md §3.5 + §5). It is a JuiceFS-style data path:
// a file is split into fixed-size chunks, each chunk is Reed-Solomon
// erasure-coded into k data + m parity shards, every shard is content-addressed
// as a SHA-256 CIDv1 (mirroring daemon/wasm/cidstore.go), and the shards are
// scattered across a ShardStore. A file survives the loss of up to m shards per
// chunk, and a corrupted shard is caught by its CID before it can corrupt output.
//
// What is REAL here (v0.1):
//   - Content-addressed chunking: deterministic SHA-256 CIDv1 (raw codec) per shard.
//   - Real Reed-Solomon erasure coding via github.com/klauspost/reedsolomon
//     (Split/Encode on Put, Reconstruct/Join on Get).
//   - Get reconstructs from any k of the k+m shards, and rejects a shard whose
//     bytes do not hash back to its CID (integrity before reconstruction).
//
// What is a documented STUB (a later step, NOT faked):
//   - Peer scatter / placement. Placement is the injectable ShardStore interface;
//     this file ships only an in-memory MemShardStore. The real path stores each
//     shard on a remote peer's free space (placement carried in the Manifest's
//     Placement field) and fetches over the QUIC data plane. That cross-node
//     wiring belongs to the composition layer, not this leaf package.
//   - Metadata transaction store (names, sizes, locks). The Manifest returned by
//     Put is the in-memory metadata for one file; persisting it in a fast
//     transactional store with concurrent-write locks (vertical 04 §3) is a
//     later step.
package dfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/ipfs/go-cid"
	"github.com/klauspost/reedsolomon"
	"github.com/multiformats/go-multihash"
)

// DefaultChunkSize is the content slice size before erasure coding. ARCHITECTURE
// §5 / vertical 04 §3 describe 64 MiB chunks → smaller blocks; for v0.1 we use a
// 1 MiB chunk so tests exercise the multi-chunk path without large allocations.
// Callers may override via Config.ChunkSize.
const DefaultChunkSize = 1 << 20 // 1 MiB

// Default erasure parameters: k data shards + m parity shards. A file survives
// the loss of any m of the k+m shards in each chunk.
const (
	DefaultDataShards   = 4
	DefaultParityShards = 2
)

// ShardCID returns the canonical content address of a shard: CIDv1 with the raw
// codec over the SHA-256 multihash of the bytes. Identical to the component CID
// scheme in daemon/wasm/cidstore.go, so the whole system speaks one CID dialect.
// The same bytes always produce the same CID; any change to the bytes changes it.
func ShardCID(shard []byte) (cid.Cid, error) {
	mh, err := multihash.Sum(shard, multihash.SHA2_256, -1)
	if err != nil {
		return cid.Undef, fmt.Errorf("dfs: hash shard: %w", err)
	}
	return cid.NewCidV1(cid.Raw, mh), nil
}

// PlacedShardStore extends ShardStore with placement-aware fetch. When a
// Manifest records where each shard lives (mesh:<peer>), Get can dial the
// owner directly instead of probing every peer.
type PlacedShardStore interface {
	ShardStore
	GetPlacedShard(c cid.Cid, placement string) ([]byte, error)
}

// The in-memory implementation below is for tests and the v0.1 single-node path.
// PEER SCATTER IS A LATER STEP — a real store puts each shard on a remote peer
// (placement recorded in Manifest.Placement) and fetches it over the data plane.
type ShardStore interface {
	// PutShard stores bytes under their CID and returns a placement hint (e.g. a
	// peer id). The hint is opaque to dfs and recorded in the Manifest.
	PutShard(c cid.Cid, shard []byte) (placement string, err error)
	// GetShard fetches the bytes stored under a CID. A miss must return an error;
	// dfs treats both a miss and a hash mismatch as "this shard is unavailable"
	// and reconstructs from the others (as long as at least k remain).
	GetShard(c cid.Cid) ([]byte, error)
}

// ChunkManifest is the per-chunk metadata: the ordered shard CIDs (the first k
// are data shards, the last m are parity), their placement hints, and the
// original (pre-padding) chunk length so Get can trim Reed-Solomon zero padding.
type ChunkManifest struct {
	ShardCIDs  []cid.Cid `json:"shard_cids"`  // length k+m, data shards first
	Placement  []string  `json:"placement"`   // length k+m, parallel to ShardCIDs
	ChunkBytes int       `json:"chunk_bytes"` // original chunk length before padding
}

// Manifest is the root descriptor of a stored file: the erasure parameters, the
// per-chunk shard CIDs + placement, the total byte length, and a RootID that
// content-addresses the manifest itself (the CID over the concatenation of every
// shard CID, in order). RootID changes if any shard CID does, so it identifies
// exactly this file content. This is the IPLD-style root the metadata store and
// /cer/fs would key on.
type Manifest struct {
	RootID       cid.Cid         `json:"root_id"`
	DataShards   int             `json:"data_shards"`   // k
	ParityShards int             `json:"parity_shards"` // m
	ChunkSize    int             `json:"chunk_size"`
	TotalBytes   int64           `json:"total_bytes"`
	Chunks       []ChunkManifest `json:"chunks"`
}

// Config tunes the chunk size and erasure parameters. The zero value is invalid;
// use DefaultConfig or set all fields.
type Config struct {
	ChunkSize    int
	DataShards   int // k
	ParityShards int // m
}

// DefaultConfig returns the v0.1 defaults (1 MiB chunks, 4+2 erasure coding).
func DefaultConfig() Config {
	return Config{ChunkSize: DefaultChunkSize, DataShards: DefaultDataShards, ParityShards: DefaultParityShards}
}

func (c Config) validate() error {
	if c.ChunkSize <= 0 {
		return errors.New("dfs: chunk size must be positive")
	}
	if c.DataShards <= 0 || c.ParityShards <= 0 {
		return errors.New("dfs: data and parity shard counts must be positive")
	}
	return nil
}

// FS is a content-addressed, erasure-coded filesystem over a ShardStore.
type FS struct {
	cfg   Config
	store ShardStore
}

// New builds an FS that scatters shards into store using cfg.
func New(store ShardStore, cfg Config) (*FS, error) {
	if store == nil {
		return nil, errors.New("dfs: nil shard store")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &FS{cfg: cfg, store: store}, nil
}

// Put reads r to EOF, chunks it, Reed-Solomon erasure-codes each chunk into
// k+m content-addressed shards, scatters every shard into the store, and returns
// the Manifest. The exact bytes can be recovered by Get from any k shards/chunk.
func (f *FS) Put(r io.Reader) (Manifest, error) {
	enc, err := reedsolomon.New(f.cfg.DataShards, f.cfg.ParityShards)
	if err != nil {
		return Manifest{}, fmt.Errorf("dfs: new encoder: %w", err)
	}

	man := Manifest{
		DataShards:   f.cfg.DataShards,
		ParityShards: f.cfg.ParityShards,
		ChunkSize:    f.cfg.ChunkSize,
	}
	var rootInput []byte // concatenated shard CIDs, for the manifest root id

	buf := make([]byte, f.cfg.ChunkSize)
	for {
		n, readErr := io.ReadFull(r, buf)
		if n > 0 {
			chunk := buf[:n]
			cm, err := f.putChunk(enc, chunk)
			if err != nil {
				return Manifest{}, err
			}
			man.Chunks = append(man.Chunks, cm)
			man.TotalBytes += int64(n)
			for _, c := range cm.ShardCIDs {
				rootInput = append(rootInput, c.Bytes()...)
			}
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return Manifest{}, fmt.Errorf("dfs: read: %w", readErr)
		}
	}

	root, err := ShardCID(rootInput) // content-address the manifest itself
	if err != nil {
		return Manifest{}, err
	}
	man.RootID = root
	return man, nil
}

// putChunk Reed-Solomon-encodes one chunk into k+m shards, content-addresses
// each, and scatters them. Split pads the last data shard with zeros; ChunkBytes
// records the true length so Join trims the padding on the way back.
func (f *FS) putChunk(enc reedsolomon.Encoder, chunk []byte) (ChunkManifest, error) {
	shards, err := enc.Split(chunk)
	if err != nil {
		return ChunkManifest{}, fmt.Errorf("dfs: split chunk: %w", err)
	}
	if err := enc.Encode(shards); err != nil {
		return ChunkManifest{}, fmt.Errorf("dfs: encode parity: %w", err)
	}

	cm := ChunkManifest{
		ShardCIDs:  make([]cid.Cid, len(shards)),
		Placement:  make([]string, len(shards)),
		ChunkBytes: len(chunk),
	}
	for i, sh := range shards {
		c, err := ShardCID(sh)
		if err != nil {
			return ChunkManifest{}, err
		}
		placement, err := f.store.PutShard(c, sh)
		if err != nil {
			return ChunkManifest{}, fmt.Errorf("dfs: store shard %s: %w", c, err)
		}
		cm.ShardCIDs[i] = c
		cm.Placement[i] = placement
	}
	return cm, nil
}

// Get reconstructs the original file from a Manifest and returns a reader over
// the exact bytes Put stored. For each chunk it fetches whatever shards are
// available, VERIFIES each fetched shard against its CID before use (a shard
// whose bytes do not hash to its recorded CID is discarded, not trusted),
// reconstructs any missing/discarded shards from the survivors via Reed-Solomon,
// and joins the k data shards back into the chunk. It succeeds as long as at
// least k valid shards per chunk remain; otherwise it returns an error.
func (f *FS) Get(man Manifest) (io.ReadCloser, error) {
	if man.DataShards <= 0 || man.ParityShards <= 0 {
		return nil, errors.New("dfs: manifest has invalid shard counts")
	}
	enc, err := reedsolomon.New(man.DataShards, man.ParityShards)
	if err != nil {
		return nil, fmt.Errorf("dfs: new encoder: %w", err)
	}
	total := man.DataShards + man.ParityShards

	var out bytes.Buffer
	for ci, cm := range man.Chunks {
		if len(cm.ShardCIDs) != total {
			return nil, fmt.Errorf("dfs: chunk %d has %d shard cids, want %d", ci, len(cm.ShardCIDs), total)
		}
		chunk, err := f.getChunk(enc, cm, man.DataShards)
		if err != nil {
			return nil, fmt.Errorf("dfs: reconstruct chunk %d: %w", ci, err)
		}
		out.Write(chunk)
	}
	return io.NopCloser(&out), nil
}

// --- random access ---------------------------------------------------------

// readCacheChunks bounds how many reconstructed chunks ONE open File retains.
// It exists because the kernel reads a mounted file in small slices (FUSE issues
// 128 KiB reads by default) while the smallest thing dfs can reconstruct is a
// whole ChunkSize chunk: without a cache, a sequential read of one 1 MiB chunk
// would re-fetch and re-decode its shards eight times over. Four chunks is
// enough to absorb the kernel's readahead reordering while bounding an open
// file's cache at 4*ChunkSize (4 MiB at the default chunk size) REGARDLESS of
// the file's total size — reading 4 GiB never buffers 4 GiB.
const readCacheChunks = 4

// File is a random-access reader over a stored file. It is what makes /cer/fs
// mountable: a FUSE/9P read arrives as (offset, count) and must be answered
// without materializing the whole file, which Get cannot do (Get reconstructs
// every chunk into one buffer, so serving a 4 KiB read of a 4 GiB file through
// it would cost 4 GiB of RAM).
//
// File instead reconstructs ONLY the chunks that overlap the requested range,
// and caches the last readCacheChunks of them. The costs, stated plainly:
//
//   - MEMORY is bounded by readCacheChunks*ChunkSize per open file plus the
//     k+m shards of the chunk being decoded — independent of file size.
//   - READ AMPLIFICATION is a whole chunk. The smallest unit dfs can verify is
//     a shard (a shard's bytes are checked against its CID; half a shard cannot
//     be), and the smallest unit it can Reconstruct is a chunk's shard set, so a
//     4 KiB read of a cold chunk fetches that chunk's shards (k+m*ChunkSize/k
//     bytes = 1.5 MiB at the defaults). Sequential reads amortize this to ~1x
//     via the cache; small random reads scattered across a large file do not.
//     A future optimization can fetch only the data shards spanning the range
//     when all of them are intact and fall back to full reconstruction
//     otherwise; this deliberately reuses the proven getChunk path instead, so
//     integrity and parity verification are identical on every read.
//   - CONCURRENCY: reads on one File serialize on its cache mutex.
//
// A File is a read-only view of the Manifest passed to Open: it resolves no
// names and re-reads no metadata, so a concurrent overwrite of the same /cer/fs
// path cannot tear an in-flight read.
type File struct {
	fs  *FS
	man Manifest
	enc reedsolomon.Encoder

	mu     sync.Mutex
	cached []cachedChunk // most-recently-used first
}

// cachedChunk is one reconstructed chunk held in File's LRU. data is never
// mutated after it is built, so a reader may copy out of it after the cache
// lock is dropped (an eviction only forgets the slice; the bytes stay valid).
type cachedChunk struct {
	idx  int
	data []byte
}

// Open returns a random-access reader over the file described by man. It
// validates the manifest up front so a malformed one fails here rather than
// silently mis-addressing bytes at read time.
func (f *FS) Open(man Manifest) (*File, error) {
	if man.DataShards <= 0 || man.ParityShards <= 0 {
		return nil, errors.New("dfs: manifest has invalid shard counts")
	}
	if man.ChunkSize <= 0 {
		return nil, errors.New("dfs: manifest has invalid chunk size")
	}
	total := man.DataShards + man.ParityShards
	for i, cm := range man.Chunks {
		if len(cm.ShardCIDs) != total {
			return nil, fmt.Errorf("dfs: chunk %d has %d shard cids, want %d", i, len(cm.ShardCIDs), total)
		}
		// Random access maps a byte offset to a chunk with off/ChunkSize, which
		// is only correct because Put fills every chunk but the last to exactly
		// ChunkSize. Verify that rather than trust it: a manifest that violates
		// it would otherwise read the wrong bytes with no error at all.
		if i < len(man.Chunks)-1 && cm.ChunkBytes != man.ChunkSize {
			return nil, fmt.Errorf(
				"dfs: chunk %d holds %d bytes but the chunk size is %d — only the final chunk may be short",
				i, cm.ChunkBytes, man.ChunkSize)
		}
	}
	enc, err := reedsolomon.New(man.DataShards, man.ParityShards)
	if err != nil {
		return nil, fmt.Errorf("dfs: new encoder: %w", err)
	}
	return &File{fs: f, man: man, enc: enc}, nil
}

// Size is the file's total length in bytes.
func (fl *File) Size() int64 { return fl.man.TotalBytes }

// Close releases the chunk cache. The File is unusable for further reads only
// in the sense that its cache is cold; it holds no OS resources.
func (fl *File) Close() error {
	fl.mu.Lock()
	fl.cached = nil
	fl.mu.Unlock()
	return nil
}

// ReadAt implements io.ReaderAt over the reconstructed file. It reconstructs
// only the chunks the requested range touches. Per the io.ReaderAt contract it
// returns a non-nil error whenever n < len(p); a read that runs off the end of
// the file returns io.EOF.
func (fl *File) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("dfs: read at negative offset")
	}
	if off >= fl.man.TotalBytes {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) {
		cur := off + int64(n)
		if cur >= fl.man.TotalBytes {
			return n, io.EOF
		}
		idx := int(cur / int64(fl.man.ChunkSize))
		within := int(cur % int64(fl.man.ChunkSize))
		chunk, err := fl.chunk(idx)
		if err != nil {
			return n, err
		}
		if within >= len(chunk) {
			// The manifest claims more bytes than its chunks hold. Report EOF
			// rather than spin: TotalBytes and the chunk lengths disagree.
			return n, io.EOF
		}
		n += copy(p[n:], chunk[within:])
	}
	return n, nil
}

// chunk returns chunk idx, reconstructing it through the same integrity-checked
// getChunk path Get uses, and caches it MRU-first.
func (fl *File) chunk(idx int) ([]byte, error) {
	fl.mu.Lock()
	defer fl.mu.Unlock()

	for i, cc := range fl.cached {
		if cc.idx == idx {
			copy(fl.cached[1:i+1], fl.cached[:i]) // move-to-front
			fl.cached[0] = cc
			return cc.data, nil
		}
	}
	if idx < 0 || idx >= len(fl.man.Chunks) {
		return nil, fmt.Errorf("dfs: chunk %d out of range (file has %d chunks)", idx, len(fl.man.Chunks))
	}
	data, err := fl.fs.getChunk(fl.enc, fl.man.Chunks[idx], fl.man.DataShards)
	if err != nil {
		return nil, fmt.Errorf("dfs: reconstruct chunk %d: %w", idx, err)
	}
	fl.cached = append([]cachedChunk{{idx: idx, data: data}}, fl.cached...)
	if len(fl.cached) > readCacheChunks {
		fl.cached = fl.cached[:readCacheChunks]
	}
	return data, nil
}

var _ io.ReaderAt = (*File)(nil)

// getChunk fetches, integrity-checks, reconstructs and joins one chunk.
func (f *FS) getChunk(enc reedsolomon.Encoder, cm ChunkManifest, k int) ([]byte, error) {
	shards := make([][]byte, len(cm.ShardCIDs))
	present := 0
	for i, c := range cm.ShardCIDs {
		var placement string
		if i < len(cm.Placement) {
			placement = cm.Placement[i]
		}
		b, err := f.getShard(c, placement)
		if err != nil {
			shards[i] = nil // unavailable — Reconstruct will try to rebuild it
			continue
		}
		// CID integrity: a corrupted or substituted shard must be rejected BEFORE
		// it can corrupt the output. Re-hash the fetched bytes; if they do not
		// produce the recorded CID, drop the shard and rely on the others.
		got, err := ShardCID(b)
		if err != nil {
			return nil, err
		}
		if !got.Equals(c) {
			shards[i] = nil
			continue
		}
		shards[i] = b
		present++
	}
	if present < k {
		return nil, fmt.Errorf("%w: have %d valid shards, need %d", reedsolomon.ErrTooFewShards, present, k)
	}

	// Rebuild whatever was missing or rejected, then verify the full set is
	// internally consistent before trusting the data shards.
	if err := enc.Reconstruct(shards); err != nil {
		return nil, err
	}
	ok, err := enc.Verify(shards)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("dfs: reconstructed shard set failed parity verification")
	}

	var chunk bytes.Buffer
	if err := enc.Join(&chunk, shards, cm.ChunkBytes); err != nil {
		return nil, fmt.Errorf("dfs: join shards: %w", err)
	}
	return chunk.Bytes(), nil
}

// getShard fetches one shard, using placement hints when the store supports them.
func (f *FS) getShard(c cid.Cid, placement string) ([]byte, error) {
	if ps, ok := f.store.(PlacedShardStore); ok && placement != "" {
		return ps.GetPlacedShard(c, placement)
	}
	return f.store.GetShard(c)
}

// MemShardStore is an in-memory ShardStore for tests and the v0.1 single-node
// path. PEER SCATTER IS A LATER STEP: a real store places each shard on a remote
// peer's free space and fetches it over the QUIC data plane (vertical 04 §3).
type MemShardStore struct {
	mu     sync.RWMutex
	blocks map[string][]byte // key: cid.KeyString()
}

// NewMemShardStore returns an empty in-memory store.
func NewMemShardStore() *MemShardStore {
	return &MemShardStore{blocks: map[string][]byte{}}
}

// PutShard copies and stores the bytes; the placement hint is "mem" (single node).
func (m *MemShardStore) PutShard(c cid.Cid, shard []byte) (string, error) {
	cp := append([]byte(nil), shard...)
	m.mu.Lock()
	m.blocks[c.KeyString()] = cp
	m.mu.Unlock()
	return "mem", nil
}

// GetShard returns a copy of the stored bytes, or an error if the CID is absent.
func (m *MemShardStore) GetShard(c cid.Cid) ([]byte, error) {
	m.mu.RLock()
	b, ok := m.blocks[c.KeyString()]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("dfs: shard %s not found", c)
	}
	return append([]byte(nil), b...), nil
}

// Drop removes a shard's bytes — used by tests to simulate shard loss. It is not
// part of the ShardStore interface.
func (m *MemShardStore) Drop(c cid.Cid) {
	m.mu.Lock()
	delete(m.blocks, c.KeyString())
	m.mu.Unlock()
}

// Corrupt flips the stored bytes for a CID to a different value WITHOUT updating
// the key — used by tests to prove CID integrity rejects a tampered shard. It is
// not part of the ShardStore interface.
func (m *MemShardStore) Corrupt(c cid.Cid, bad []byte) {
	m.mu.Lock()
	m.blocks[c.KeyString()] = append([]byte(nil), bad...)
	m.mu.Unlock()
}
