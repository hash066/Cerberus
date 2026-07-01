// /cer/fs — the distributed filesystem subtree of the 9P namespace.
//
// This file wires the content-addressed, erasure-coded filesystem engine
// (daemon/dfs, Phase G2) into the capability-gated 9P namespace so that
// /cer/fs/<path> is a real, capability-checked file you can write and read.
//
// The defining invariant of vertical 04 (ARCHITECTURE.md §3.5) is preserved
// exactly: 9P read/write NEVER carries bulk file bytes. Opening a /cer/fs path
// returns a DATA-PLANE ENDPOINT (a DataEndpoint, just like a device `.../ctl`),
// and the file bytes flow over the QUIC data plane referenced by that endpoint —
// never over the 9P wire. The 9P layer only names the file, checks the
// capability, and mints the grant; the bulk bytes ride the data plane.
//
// Direction of flow (the data plane is send-only: a client sends to a server):
//   - WRITE /cer/fs/<path>: the 9P caller HAS the bytes and wants the daemon to
//     store them. The caller becomes the data-plane sender; the daemon's
//     data-plane receiver streams the bytes straight into dfs.Put and records the
//     returned Manifest keyed by <path>. This is the natural fit for the existing
//     receive-only data plane and is fully real.
//   - READ /cer/fs/<path>: the daemon HAS the bytes (it resolves <path> to its
//     Manifest and runs dfs.Get) and the caller wants them. Whoever holds the
//     bytes sends them, so the daemon becomes the data-plane sender and streams
//     dfs.Get's output to a receiver the caller supplies (a RecvEndpoint: the
//     caller runs its own data-plane receiver and registers an inbound grant on
//     it). The daemon dials that receiver and sends. No file byte touches 9P.
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

// FSStore is the seam the composition layer implements to back /cer/fs with the
// real filesystem engine (daemon/dfs) and the real data plane (daemon/dataplane).
// The 9P namespace calls it after a capability check; it owns all dfs + data-plane
// mechanics so this package couples to neither.
//
// Bulk file content always moves over the data plane, never over 9P, upholding
// the vertical 04 §3.5 invariant.
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
