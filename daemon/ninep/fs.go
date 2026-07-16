// /cer/fs — the distributed filesystem subtree of the 9P namespace.
//
// This file wires the content-addressed, erasure-coded filesystem engine
// (daemon/dfs, Phase G2) into the capability-gated 9P namespace so that
// /cer/fs/<path> is a real, capability-checked file you can write and read.
//
// TWO READ PATHS, AND WHY (read this before "simplifying" one away).
//
// The invariant vertical 04 states is precise, and it is about DEVICES: "Tensor/
// VRAM bytes never traverse 9P read/write; opening a CONTROL FILE returns a
// data-plane endpoint". Its rejected-alternatives table names the excluded design
// exactly: «Naive "VRAM as a file you read()"». That invariant is intact and
// untouched here — a device `.../ctl` still returns a DataEndpoint and never
// bytes.
//
// /cer/fs is a different thing, and the architecture says so: §3.5 lists it as
// "IPLD/Reed-Solomon distributed filesystem" and vertical 04 §3 specifies it as a
// JuiceFS-style split. JuiceFS is a filesystem you MOUNT and read() — a
// distributed filesystem whose files cannot be read by `cat` is not a filesystem.
// So /cer/fs supports both:
//
//   - BULK, out-of-band (BeginRead/OpenFSRead + RecvEndpoint). The daemon holds
//     the bytes, so it is the data-plane SENDER: it dials a receiver the caller
//     supplies and streams dfs.Get's output there. Zero file bytes touch 9P. This
//     is what the CLI/gateway drive for whole-file transfers, and it stays the
//     path for anything large moving between nodes.
//   - RANGED, in-band (OpenFSReader + FSReader.ReadAt). A mounted filesystem
//     cannot drive the path above: the kernel's read(2) arrives as (offset,
//     count) and wants bytes in a buffer, and there is nowhere in that syscall to
//     hand back "here is a QUIC endpoint, go dial it". A RecvEndpoint read can
//     only be driven by a bespoke client that stands up its own receiver — never
//     by `cat`. So a ranged read answers from the dfs engine directly, and the
//     bytes ride the 9P/FUSE reply for the last local hop into the caller.
//
// What that last hop does NOT do is turn 9P into the cluster's data plane: the
// CROSS-NODE traffic is still shard fetches over the mesh (see the ShardStore in
// daemon/system), exactly as before. 9P carries only the final hop out of the
// daemon that already holds the reconstructed bytes — the same hop JuiceFS's own
// FUSE mount performs. The ranged read is also chunk-granular, not whole-file
// (daemon/dfs's File), so it costs one erasure-coded chunk, not the file.
//
// WRITE /cer/fs/<path>: the 9P caller HAS the bytes and wants the daemon to store
// them. The caller becomes the data-plane sender; the daemon's data-plane
// receiver streams the bytes straight into dfs.Put and records the returned
// Manifest keyed by <path>. This is the natural fit for the receive-only data
// plane and is fully real. NOTE that this is an out-of-band write only: writing
// THROUGH a mount (`cp x /mnt/cerberus/fs/x`) does not work, because the 9P node
// is a templatefs.ReadOnlyFile whose WriteAt is EROFS and whose Create is
// ENOTDIR. That is reported, not faked — see wire.go's node.Open.
//
// The mechanics (dfs + dataplane) live entirely in the composition layer
// (daemon/system), injected here through the FSStore seam, so this package stays
// decoupled from both dfs and dataplane (no import cycle, same pattern as the
// device Granter). The namespace allocates the transfer id (from the same stream
// counter devices use) so all grants on the daemon's data-plane server share one
// id space. When no FSStore is wired, the /cer/fs operations report PARTITIONED —
// the namespace stays usable for devices without a filesystem backend, and
// existing callers are unaffected.
//
// WHAT IS A DOCUMENTED STUB (a later step, NOT faked):
//   - The path→Manifest map is in-memory (see MemMetaStore in daemon/system).
//     A durable, transactional metadata store (names, sizes, locks — vertical 04
//     §3) is the next step; it is labelled as such where it is constructed.
//   - Peer scatter of shards is the dfs ShardStore's concern (MemShardStore for
//     now); see daemon/dfs and daemon/system.

package ninep

import (
	"strings"

	contract "github.com/hash066/cerberus/contract/go"
)

// FSRoot is the mount point of the distributed filesystem in the namespace.
const FSRoot = "/cer/fs"

// RecvEndpoint describes the caller's OWN data-plane receiver for a /cer/fs read.
// Because the data plane is send-only, a read makes the daemon the sender; the
// caller must therefore stand up a receiver and register an inbound grant on it,
// then hand the daemon this descriptor. It mirrors dataplane.Endpoint but is
// defined here so ninep does not import daemon/dataplane (no coupling).
type RecvEndpoint struct {
	// Kind is the transport family (only "quic" is served in v0.1).
	Kind EndpointKind `json:"kind"`
	// Endpoint is the caller's receiver transport address the daemon dials.
	Endpoint string `json:"endpoint"`
	// StreamID is the transfer id the caller registered on its receiver.
	StreamID uint64 `json:"stream_id"`
	// Cap is the capability the caller registered that inbound grant under; the
	// daemon presents it in the transfer header so the caller's receiver
	// authorizes the delivery. This is the caller's inbound cap, not the read cap.
	Cap contract.CapHandle `json:"cap"`
	// Quota bounds the inbound transfer the caller authorized (must be >= file
	// size or the caller's receiver rejects the delivery).
	Quota contract.Quota `json:"quota"`
	// ServerPeerID, if set, is the Ed25519 PeerID the caller's OWN receiver
	// presents in its TLS certificate. When known, the daemon (acting as the
	// data-plane CLIENT for a read — see BeginRead) pins its dial to this key,
	// so a MITM impersonating the caller's receiver is rejected before any file
	// byte is sent. This is optional: a caller that stood up an ad hoc receiver
	// without a durable identity leaves this zero, and the transfer is then
	// authorized only by the in-band capability check (Cap/Quota above) — a
	// documented, not silently unsafe, gap (see daemon/dataplane/tls.go).
	ServerPeerID contract.PeerID `json:"server_peer_id,omitempty"`
}

// FSEntry is one file stored in /cer/fs: its full namespace path and its size in
// bytes. It is what enumeration (FSList → Readdir) and stat (FSStat → GetAttr)
// return, so `ls -l` through a mount reports real names and real sizes.
type FSEntry struct {
	// Path is the full namespace path, e.g. "/cer/fs/notes.txt".
	Path string `json:"path"`
	// Size is the file's length in bytes (the dfs Manifest's TotalBytes).
	Size int64 `json:"size"`
}

// FSReader is an open, random-access view of ONE /cer/fs file, handed back by
// FSStore.OpenRead after the namespace has capability-checked the path.
//
// It is a ReaderAt and not a Reader because that is the shape of the question a
// mounted filesystem asks: the kernel reads a file as (offset, count), never as
// a stream from zero. Backing it with an io.ReaderAt lets the dfs engine
// reconstruct only the erasure-coded chunks a read actually touches, so serving
// a 4 KiB read of a 4 GiB file costs one chunk, not the whole file (see
// daemon/dfs's File).
type FSReader interface {
	// ReadAt fills p from off, per the io.ReaderAt contract (a short read
	// always carries a non-nil error; reading past the end gives io.EOF).
	ReadAt(p []byte, off int64) (int, error)
	// Size is the file's total length in bytes.
	Size() int64
	// Close releases the reader's buffers.
	Close() error
}

// FSStore is the seam the composition layer implements to back /cer/fs with the
// real filesystem engine (daemon/dfs) and the real data plane (daemon/dataplane).
// The 9P namespace calls it after a capability check; it owns all dfs + data-plane
// mechanics so this package couples to neither.
type FSStore interface {
	// BeginWrite authorizes storing a file at path. It registers a
	// capability-bound data-plane transfer (under the namespace-assigned
	// transferID) whose received bytes are streamed into dfs.Put; on completion
	// the returned Manifest is recorded keyed by path. The returned endpoint is
	// where the 9P caller SENDS the file bytes.
	BeginWrite(path string, cap contract.CapHandle, transferID uint64) (DataEndpoint, error)
	// BeginRead resolves path to its Manifest, runs dfs.Get, and streams the
	// reconstructed bytes to the caller's receiver (recv). The daemon is the
	// data-plane sender for a read. It returns once the transfer completes (or
	// fails). An unknown path is denied.
	BeginRead(path string, cap contract.CapHandle, recv RecvEndpoint) error
	// List returns every file stored in /cer/fs. The namespace filters the
	// result down to the entries the asking capability may read before any name
	// is exposed (see Server.FSList / Server.ListChildren) — List itself does no
	// capability check and must never be reachable except through them.
	List() ([]FSEntry, error)
	// Stat returns the entry for exactly path, or an error if no file was
	// written there. Like List, it is called only after a capability check.
	Stat(path string) (FSEntry, error)
	// OpenRead returns a random-access reader over the file at path, or an error
	// if no file was written there. Like List, it is called only after a
	// capability check. The caller must Close the reader.
	OpenRead(path string) (FSReader, error)
}

// SetFSStore installs the /cer/fs backend. Call it once at composition time,
// before the namespace is served.
func (s *Server) SetFSStore(store FSStore) {
	s.mu.Lock()
	s.fs = store
	s.mu.Unlock()
}

// fsResource is the ResourceRef for a /cer/fs path. Every walk/open under
// /cer/fs is capability-checked against a resource of this shape (kind "fs",
// the concrete path), exactly like a device is checked against its ResourceRef.
func fsResource(path string) contract.ResourceRef {
	return contract.ResourceRef{Kind: contract.KindFS, Path: path}
}

// isFSPath reports whether path is under the /cer/fs subtree (the root itself or
// a file beneath it).
func isFSPath(path string) bool {
	path = strings.TrimRight(path, "/")
	return path == FSRoot || strings.HasPrefix(path, FSRoot+"/")
}

// fsSubPath returns the file path beneath /cer/fs (e.g. "docs/a.txt" for
// "/cer/fs/docs/a.txt"), and whether path is a real file path (not the root).
func fsSubPath(path string) (string, bool) {
	path = strings.TrimRight(path, "/")
	if !strings.HasPrefix(path, FSRoot+"/") {
		return "", false
	}
	sub := strings.TrimPrefix(path, FSRoot+"/")
	if sub == "" {
		return "", false
	}
	return sub, true
}

// WalkFS resolves a /cer/fs path, gated by a read capability on the fs resource.
// The root (/cer/fs) is traversable without naming a file; a file path is
// read-checked. A capability is required to walk to any file, exactly as devices
// require a capability to be seen.
func (s *Server) WalkFS(path string, cap contract.CapHandle) error {
	if strings.TrimRight(path, "/") == FSRoot {
		// The fs root is a structural directory; traversing to it exposes no file
		// bytes. The capability check fires at the file boundary (below).
		return nil
	}
	sub, ok := fsSubPath(path)
	if !ok {
		return contract.Errf(contract.ErrDenied, "not an fs path: "+path)
	}
	return s.check(cap, "read", fsResource(FSRoot+"/"+sub))
}

// OpenFSWrite opens a /cer/fs file for writing and returns the data-plane
// endpoint the caller SENDS the file bytes to. It requires a write capability on
// the fs resource; the bytes stream into dfs.Put over the data plane (never over
// 9P) and the resulting Manifest is recorded keyed by the path.
func (s *Server) OpenFSWrite(path string, cap contract.CapHandle) (DataEndpoint, error) {
	sub, ok := fsSubPath(path)
	if !ok {
		return DataEndpoint{}, contract.Errf(contract.ErrDenied, "not an fs file path: "+path)
	}
	full := FSRoot + "/" + sub
	if err := s.check(cap, "write", fsResource(full)); err != nil {
		return DataEndpoint{}, err
	}
	s.mu.Lock()
	store := s.fs
	s.nextStream++
	transferID := s.nextStream
	s.mu.Unlock()
	if store == nil {
		return DataEndpoint{}, contract.Errf(contract.ErrPartitioned,
			"/cer/fs has no filesystem backend wired (SetFSStore); the dfs+data-plane bridge is installed by daemon/system.Compose")
	}
	return store.BeginWrite(full, cap, transferID)
}

// fsBackend returns the wired FSStore, or the PARTITIONED error every /cer/fs
// entry point reports when the composition layer never installed one.
func (s *Server) fsBackend() (FSStore, error) {
	s.mu.Lock()
	store := s.fs
	s.mu.Unlock()
	if store == nil {
		return nil, contract.Errf(contract.ErrPartitioned,
			"/cer/fs has no filesystem backend wired (SetFSStore); the dfs+data-plane bridge is installed by daemon/system.Compose")
	}
	return store, nil
}

// FSList returns every file in /cer/fs that cap is authorized to READ — the
// enumeration counterpart to WalkFS, and the source ListChildren (and therefore
// a mount's Readdir) draws /cer/fs entries from.
//
// The filter is the load-bearing part: each candidate is put through the SAME
// check WalkFS would apply to it (read on that file's own fsResource), so a
// listing can never name a file the capability could not walk to. A capability
// scoped to /cer/fs sees every file beneath it; one scoped to /cer/fs/a.txt sees
// only a.txt; one scoped to a device sees nothing here at all.
func (s *Server) FSList(cap contract.CapHandle) ([]FSEntry, error) {
	store, err := s.fsBackend()
	if err != nil {
		return nil, err
	}
	all, err := store.List()
	if err != nil {
		return nil, err
	}
	out := make([]FSEntry, 0, len(all))
	for _, e := range all {
		if s.check(cap, "read", fsResource(e.Path)) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// FSIsDir reports whether path names a DIRECTORY within /cer/fs — that is, the
// root itself, or a path some file this capability may read lives beneath.
//
// /cer/fs paths are logical: the metadata store records FILES, never directories,
// so a directory exists exactly when it contains one. That also means an empty
// directory cannot exist, and a directory a capability may not read into is
// indistinguishable from one that is not there — both of which are the intended
// answers rather than gaps.
func (s *Server) FSIsDir(path string, cap contract.CapHandle) bool {
	path = strings.TrimRight(path, "/")
	if path == FSRoot {
		return true
	}
	if !isFSPath(path) {
		return false
	}
	// A real file AT this exact path is a file, not a directory. Asking this
	// first keeps the common case — walking to a file — down to a single
	// metadata lookup, instead of enumerating the whole store (which, against
	// the replicated store, fans out to mesh peers) on every walk hop.
	if _, err := s.FSStat(path, cap); err == nil {
		return false
	}
	entries, err := s.FSList(cap)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Path, path+"/") {
			return true
		}
	}
	return false
}

// FSStat returns the entry for one /cer/fs file, gated by a read capability on
// it. A path that was never written is reported as not existing (rather than as
// an empty file), which is what stops a mount from showing phantom zero-byte
// files for invented names — the /cer/fs analogue of the device phantom-path fix
// documented on Server.Walk.
func (s *Server) FSStat(path string, cap contract.CapHandle) (FSEntry, error) {
	sub, ok := fsSubPath(path)
	if !ok {
		return FSEntry{}, contract.Errf(contract.ErrDenied, "not an fs file path: "+path)
	}
	full := FSRoot + "/" + sub
	if err := s.check(cap, "read", fsResource(full)); err != nil {
		return FSEntry{}, err
	}
	store, err := s.fsBackend()
	if err != nil {
		return FSEntry{}, err
	}
	return store.Stat(full)
}

// OpenFSReader opens a /cer/fs file for RANGED, in-band reading and returns a
// random-access reader over it, gated by a read capability on the file. It is
// the read path a MOUNT uses: the kernel asks for (offset, count) and this
// answers with bytes, reconstructing only the erasure-coded chunks the range
// touches (see the package note above for why the out-of-band OpenFSRead cannot
// serve a mount, and daemon/dfs's File for the cost of this one).
//
// A path that was never written is denied — a read of a path that does not exist
// fails, it does not fabricate bytes. The caller must Close the reader.
func (s *Server) OpenFSReader(path string, cap contract.CapHandle) (FSReader, error) {
	sub, ok := fsSubPath(path)
	if !ok {
		return nil, contract.Errf(contract.ErrDenied, "not an fs file path: "+path)
	}
	full := FSRoot + "/" + sub
	if err := s.check(cap, "read", fsResource(full)); err != nil {
		return nil, err
	}
	store, err := s.fsBackend()
	if err != nil {
		return nil, err
	}
	return store.OpenRead(full)
}

// OpenFSRead opens a /cer/fs file for reading. The daemon holds the bytes, so it
// is the data-plane sender: it resolves the path to its Manifest, runs dfs.Get,
// and streams the reconstructed bytes to the receiver the caller supplies in
// recv. It requires a read capability on the fs resource. An unknown path is
// denied — a read of a path that was never written fails, it does not fabricate
// bytes. Returns nil once the file has been delivered over the data plane.
func (s *Server) OpenFSRead(path string, cap contract.CapHandle, recv RecvEndpoint) error {
	sub, ok := fsSubPath(path)
	if !ok {
		return contract.Errf(contract.ErrDenied, "not an fs file path: "+path)
	}
	full := FSRoot + "/" + sub
	if err := s.check(cap, "read", fsResource(full)); err != nil {
		return err
	}
	s.mu.Lock()
	store := s.fs
	s.mu.Unlock()
	if store == nil {
		return contract.Errf(contract.ErrPartitioned,
			"/cer/fs has no filesystem backend wired (SetFSStore); the dfs+data-plane bridge is installed by daemon/system.Compose")
	}
	return store.BeginRead(full, cap, recv)
}
