// 9P2000.L wire transport for the capability-gated namespace.
//
// This file serves the EXISTING in-memory Server (Walk/Open/ReadInfo, see
// ninep.go) over the real 9P2000.L protocol using github.com/hugelgupf/p9, so
// that peers — and eventually a FUSE/WinFsp mount (see mount.go) — can walk and
// open the namespace over the wire. The protocol encode/decode is the real
// library; the capability gating and the ctl-returns-endpoint invariant are the
// existing Server's, unchanged.
//
// How the capability is carried (design decision):
//
//	hugelgupf/p9's Attacher.Attach() takes NO arguments — the 9P uname/aname
//	fields are parsed off the wire but discarded before Attach() is called, and
//	the library documents that "authentication is not currently supported". A
//	CapHandle is a uint64 in-process table index; it is meaningless across a real
//	network socket anyway (it names a slot in THIS node's kernel only). So we
//	bind the capability to the CONNECTION: every accepted 9P connection is served
//	by an Attacher pinned to exactly one principal's capabilities. This realises
//	vertical 04 §7 ("the namespace is per-principal: a node only sees device
//	files for which it holds a capability") — one connection == one principal ==
//	one view. EVERY Twalk/Topen on that connection is still checked through the
//	existing Server against that connection's capabilities; there is no ambient
//	authority. A future networked transport would, at connection-accept time,
//	exchange a signed CBOR capability (schemas/capability.cddl) and translate it
//	into a local CapHandle before constructing the Attacher — the seam is the
//	same.
//
//	A principal is a capability SET, not a single handle (see capSet): the kernel
//	scopes each capability to one Kind at one path, so presenting both a device
//	and /cer/fs on one connection necessarily means naming both capabilities.
//	Serve/Handle/DialCap keep their single-handle signatures (a set of one);
//	ServeCaps/HandleCaps/DialCaps take the full keyring.
package ninep

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/hugelgupf/p9/fsimpl/templatefs"
	"github.com/hugelgupf/p9/linux"
	"github.com/hugelgupf/p9/p9"

	contract "github.com/hash066/cerberus/contract/go"
)

// capSet is the set of capabilities ONE 9P connection — and therefore one mount
// — is bound to: a principal's keyring.
//
// Why a set and not a handle. A ResourceRef names one Kind at one path, and the
// kernel checks scope exactly (contract/go/stub's sameResource: Kind must match,
// and the granted path must cover the requested one). So NO single capability
// can authorize both /cer/dev/vram/local/0 (KindVRAM) and /cer/fs (KindFS) —
// that is the kernel working as designed, refusing to mint a god-cap. A
// principal entitled to both therefore HOLDS TWO capabilities, and a connection
// that is to present both must name both.
//
// This adds no ambient authority. Every access is still authorized by ONE
// specific capability that genuinely covers that exact resource; the set's view
// is exactly the union of what its members separately authorize, an empty set
// authorizes nothing, and revoking a member removes its share. It is a keyring,
// not a widening: `try` never combines two capabilities to permit something
// neither permits alone.
type capSet []contract.CapHandle

// try runs fn against each capability and succeeds if ANY of them authorizes the
// access. When they all refuse it reports a real denial from a real handle
// rather than a synthesized one, preferring an ABSENCE: the capabilities that do
// not cover the resource each report a scope denial, and surfacing one of those
// would tell a shell "permission denied" for a path that merely does not exist.
func (cs capSet) try(fn func(contract.CapHandle) error) error {
	if len(cs) == 0 {
		return contract.Errf(contract.ErrDenied, "no capability presented")
	}
	var worst error
	for _, c := range cs {
		err := fn(c)
		if err == nil {
			return nil
		}
		if worst == nil || toErrno(err) == linux.ENOENT {
			worst = err
		}
	}
	return worst
}

// tryVal is try for an operation that yields a value (Open, ReadInfo, ...).
func tryVal[T any](cs capSet, fn func(contract.CapHandle) (T, error)) (T, error) {
	var zero T
	if len(cs) == 0 {
		return zero, contract.Errf(contract.ErrDenied, "no capability presented")
	}
	var worst error
	for _, c := range cs {
		v, err := fn(c)
		if err == nil {
			return v, nil
		}
		if worst == nil || toErrno(err) == linux.ENOENT {
			worst = err
		}
	}
	return zero, worst
}

// any reports whether some capability satisfies the predicate — the read-only
// form used to classify a path (e.g. FSIsDir) where refusal is not an error.
func (cs capSet) any(fn func(contract.CapHandle) bool) bool {
	for _, c := range cs {
		if fn(c) {
			return true
		}
	}
	return false
}

// WireServer adapts the capability-gated Server onto the 9P2000.L wire. It is a
// thin transport edge: all authorization stays in Server.
type WireServer struct {
	ns *Server
}

// NewWireServer wraps an existing capability-gated namespace for 9P serving.
func NewWireServer(ns *Server) *WireServer { return &WireServer{ns: ns} }

// Serve accepts 9P connections on l, each bound to the given capability. In a
// networked deployment the capability would be negotiated per-connection at
// accept time (see the package note above); here cap is supplied by the caller.
func (w *WireServer) Serve(l net.Listener, cap contract.CapHandle) error {
	return w.ServeCaps(l, cap)
}

// ServeCaps is Serve for a connection bound to a SET of capabilities (see
// capSet): the view it presents is the union of what they separately authorize.
func (w *WireServer) ServeCaps(l net.Listener, caps ...contract.CapHandle) error {
	srv := p9.NewServer(&attacher{ns: w.ns, caps: caps})
	return srv.Serve(l)
}

// Handle serves a single 9P connection over the given read/write pipe, bound to
// cap. Used by DialCap for the in-process loopback transport and usable by any
// caller that already has an accepted connection.
func (w *WireServer) Handle(r io.ReadCloser, wr io.WriteCloser, cap contract.CapHandle) error {
	return w.HandleCaps(r, wr, cap)
}

// HandleCaps is Handle for a connection bound to a SET of capabilities.
func (w *WireServer) HandleCaps(r io.ReadCloser, wr io.WriteCloser, caps ...contract.CapHandle) error {
	srv := p9.NewServer(&attacher{ns: w.ns, caps: caps})
	return srv.Handle(r, wr)
}

// DialCap starts an in-process 9P2000.L server for ns over an in-memory socket
// pair, bound to cap, and returns a connected 9P client attached at the root.
// This is the loopback path used by tests and by same-host components; the wire
// format on the pipe is the real 9P2000.L encoding, not a shortcut.
func DialCap(ns *Server, cap contract.CapHandle) (*p9.Client, func() error, error) {
	return DialCaps(ns, cap)
}

// DialCaps is DialCap for a connection bound to a SET of capabilities (see
// capSet) — what a mount holding a keyring dials.
func DialCaps(ns *Server, caps ...contract.CapHandle) (*p9.Client, func() error, error) {
	c1, c2 := net.Pipe()
	w := NewWireServer(ns)
	go func() { _ = w.HandleCaps(c2, c2, caps...) }()
	cl, err := p9.NewClient(c1)
	if err != nil {
		_ = c1.Close()
		_ = c2.Close()
		return nil, nil, err
	}
	closer := func() error {
		err := cl.Close()
		_ = c1.Close()
		return err
	}
	return cl, closer, nil
}

// --- server-side file tree ------------------------------------------------

// rootPath is the namespace root mounted by Attach.
const rootPath = "/cer"

// attacher binds one connection to one capability set (usually of size one).
type attacher struct {
	ns   *Server
	caps capSet
}

// Attach returns the namespace root for this connection's capabilities.
func (a *attacher) Attach() (p9.File, error) {
	return &node{ns: a.ns, caps: a.caps, path: rootPath, dir: true}, nil
}

// node is one path in the namespace. dir distinguishes a directory (walkable,
// not byte-readable) from a leaf (ctl/info). data is set on a leaf after Open so
// ReadAt can serve the descriptor bytes (endpoint JSON for ctl, info JSON for
// info) — never device bytes; the device data plane is the QUIC/RDMA endpoint
// the ctl descriptor points at.
// Close is defined below rather than inherited from templatefs.NilCloser,
// because a read-opened /cer/fs node owns an FSReader whose chunk buffers must
// be released.
type node struct {
	templatefs.ReadOnlyFile
	templatefs.NotLockable

	ns     *Server
	caps   capSet
	path   string
	dir    bool
	fsFile bool   // a /cer/fs file leaf (see Open)
	data   []byte // populated on Open for openable leaves
	fsRead FSReader
}

// qid derives a stable QID from the path. Path uniqueness is best-effort (FNV
// hash); the 9P client treats it as opaque.
func (n *node) qid() p9.QID {
	t := p9.TypeRegular
	if n.dir {
		t = p9.TypeDir
	}
	return p9.QID{Type: t, Path: fnv64(n.path)}
}

func (n *node) mode() p9.FileMode {
	if n.dir {
		return p9.ModeDirectory | 0o555
	}
	if n.fsFile {
		// A /cer/fs file is writable (open-for-write → dfs.Put via the data plane);
		// the bytes never traverse 9P, only the endpoint descriptor does.
		return p9.ModeRegular | 0o644
	}
	return p9.ModeRegular | 0o444
}

// GetAttr reports node attributes. Required by Attach (it calls GetAttr) and by
// clients before open.
//
// For a /cer/fs file this is what makes `ls -l` truthful: the size comes from
// the stored Manifest, and a path that was never written reports ENOENT instead
// of a phantom zero-byte file. (Before Open there is no descriptor to measure,
// and WalkFS authorizes a path without asserting it exists, so without this
// every invented name under fs/ would stat as an empty file — the /cer/fs
// analogue of the device phantom-path bug documented on Server.Walk.)
func (n *node) GetAttr(req p9.AttrMask) (p9.QID, p9.AttrMask, p9.Attr, error) {
	size := uint64(len(n.data))
	switch {
	case n.fsRead != nil:
		// Opened for a ranged read: the reader already knows the size.
		size = uint64(n.fsRead.Size())
	case n.fsFile && n.data == nil:
		// Not opened, or opened for read: report the stored file's real size.
		// (A write-open captures the endpoint descriptor into data, and its
		// length is what the caller must read — so that case keeps len(n.data).)
		e, err := tryVal(n.caps, func(c contract.CapHandle) (FSEntry, error) { return n.ns.FSStat(n.path, c) })
		if err != nil {
			return p9.QID{}, p9.AttrMask{}, p9.Attr{}, toErrno(err)
		}
		size = uint64(e.Size)
	}
	attr := p9.Attr{Mode: n.mode(), Size: size, NLink: 1}
	mask := p9.AttrMask{Mode: true, NLink: true, Size: true}
	return n.qid(), mask, attr, nil
}

// Close releases a read-opened /cer/fs file's chunk cache. Every other node
// holds nothing that needs closing (which is what templatefs.NilCloser assumed
// before /cer/fs reads existed).
func (n *node) Close() error {
	if n.fsRead == nil {
		return nil
	}
	err := n.fsRead.Close()
	n.fsRead = nil
	return err
}

// WalkGetAttr returns ENOSYS so the client issues Walk and GetAttr separately;
// that keeps the single capability-checked Walk path authoritative.
func (n *node) WalkGetAttr([]string) ([]p9.QID, p9.File, p9.AttrMask, p9.Attr, error) {
	return nil, nil, p9.AttrMask{}, p9.Attr{}, linux.ENOSYS
}

// StatFS reports a minimal, read-only filesystem summary. The namespace is a
// control plane, not a data store, so the figures are nominal.
func (n *node) StatFS() (p9.FSStat, error) {
	return p9.FSStat{Type: 0x01021997, BlockSize: 4096, NameLength: 255}, nil
}

// Walk resolves names from this node. Every hop into a registered device path is
// capability-checked through the existing Server (Walk uses the "read" right).
// An empty names slice clones the current node (9P "walk to self").
func (n *node) Walk(names []string) ([]p9.QID, p9.File, error) {
	if len(names) == 0 {
		clone := *n
		clone.data = nil
		// The clone is an independent fid: it must not inherit (and later Close)
		// the original's fs reader.
		clone.fsRead = nil
		return []p9.QID{}, &clone, nil
	}
	cur := n
	qids := make([]p9.QID, 0, len(names))
	for _, name := range names {
		if name == "" || name == "." || strings.Contains(name, "/") {
			return nil, nil, linux.EINVAL
		}
		next := &node{ns: n.ns, caps: n.caps, path: joinPath(cur.path, name)}
		leaf := isLeaf(name)
		next.dir = !leaf
		switch {
		case isFSPath(next.path):
			// The /cer/fs subtree is served by the namespace's WalkFS/OpenFS*
			// methods, not the device path. Every hop into a file is cap-checked
			// (WalkFS uses "read"); the /cer/fs root itself is a structural dir.
			if err := n.caps.try(func(c contract.CapHandle) error { return n.ns.WalkFS(next.path, c) }); err != nil {
				return nil, nil, toErrno(err)
			}
			// Classify: the root and any component that real files live beneath
			// are directories; everything else is a file leaf. /cer/fs paths are
			// logical (the metadata store holds files, not directories), so a
			// directory exists exactly when it contains a file this capability
			// can read — which is also why a nested path like /cer/fs/docs/a.txt
			// walks correctly. Assuming "anything that is not the root is a
			// file" (the previous rule) made `docs` a file and broke the walk.
			next.dir = n.caps.any(func(c contract.CapHandle) bool { return n.ns.FSIsDir(next.path, c) })
			next.fsFile = !next.dir
		case leaf:
			// ctl/info/alloc are leaves; the parent must be a registered device
			// and the cap must authorize read on it (Server.Walk = "read"
			// check) — so a capability is required even to SEE the leaf. The
			// stronger alloc/read gate for the actual endpoint/bytes fires at
			// Open.
			if err := n.caps.try(func(c contract.CapHandle) error { return n.ns.Walk(cur.path, c) }); err != nil {
				return nil, nil, toErrno(err)
			}
		case n.ns.IsAncestorDir(next.path):
			// Structural ancestor of some device (e.g. /cer/dev/vram): no
			// resource of its own, traversable without a capability, exposes no
			// bytes. The cap check still fires once we reach the device itself.
		default:
			// A registered device directory (or unknown path): capability-check
			// read through the existing Server. Unknown paths are denied here.
			if err := n.caps.try(func(c contract.CapHandle) error { return n.ns.Walk(next.path, c) }); err != nil {
				return nil, nil, toErrno(err)
			}
		}
		qids = append(qids, next.qid())
		cur = next
	}
	return qids, cur, nil
}

// Open enforces the core invariant on the wire:
//   - ".../ctl"  → Server.Open returns a DataEndpoint; we serve its JSON
//     descriptor (the data-plane handle), NEVER device bytes.
//   - ".../info" → Server.ReadInfo returns the static descriptor bytes.
//   - anything else (a directory, or any other leaf such as "alloc") → denied.
//
// Each branch re-checks the capability through the existing Server (alloc for
// ctl, read for info). There is no path that yields raw device bytes over 9P.
func (n *node) Open(mode p9.OpenFlags) (p9.QID, uint32, error) {
	if n.dir {
		return p9.QID{}, 0, linux.EISDIR
	}
	// /cer/fs file: opening it hands back a data-plane endpoint (the file bytes
	// ride the data plane, never 9P). A write open (WriteOnly/ReadWrite) allocates
	// a send endpoint the caller streams the file into (→ dfs.Put). A read open
	// needs the caller's own receiver endpoint, which a plain 9P Open cannot carry;
	// the read path is exposed through the Server API (Server.OpenFSRead), which
	// the CLI/gateway drive — so a bare read-open over the wire is refused here
	// with a clear errno rather than silently returning nothing.
	if n.fsFile {
		switch mode.Mode() {
		case p9.WriteOnly, p9.ReadWrite:
			ep, err := tryVal(n.caps, func(c contract.CapHandle) (DataEndpoint, error) { return n.ns.OpenFSWrite(n.path, c) })
			if err != nil {
				return p9.QID{}, 0, toErrno(err)
			}
			b, err := json.Marshal(ep)
			if err != nil {
				return p9.QID{}, 0, linux.EIO
			}
			n.data = b
			return n.qid(), 0, nil
		default:
			// A read-open serves the file's bytes, ranged. This previously
			// returned ENOSYS ("read requires an out-of-band receiver
			// endpoint"), which made /cer/fs unreadable through any mount: a
			// read(2) has nowhere to put a receiver endpoint, so the
			// RecvEndpoint path (Server.OpenFSRead) can only ever be driven by a
			// bespoke client, never by `cat`. Both paths now exist — see the
			// note in fs.go for which is which and why the device
			// ctl-returns-an-endpoint invariant is untouched by this.
			rd, err := tryVal(n.caps, func(c contract.CapHandle) (FSReader, error) { return n.ns.OpenFSReader(n.path, c) })
			if err != nil {
				return p9.QID{}, 0, toErrno(err)
			}
			n.fsRead = rd
			return n.qid(), 0, nil
		}
	}
	switch basePath(n.path) {
	case "ctl":
		ep, err := tryVal(n.caps, func(c contract.CapHandle) (DataEndpoint, error) { return n.ns.Open(n.path, c) })
		if err != nil {
			return p9.QID{}, 0, toErrno(err)
		}
		b, err := json.Marshal(ep)
		if err != nil {
			return p9.QID{}, 0, linux.EIO
		}
		n.data = b
	case "info":
		b, err := tryVal(n.caps, func(c contract.CapHandle) ([]byte, error) { return n.ns.ReadInfo(n.path, c) })
		if err != nil {
			return p9.QID{}, 0, toErrno(err)
		}
		n.data = b
	default:
		return p9.QID{}, 0, linux.EACCES
	}
	return n.qid(), 0, nil
}

// Create creates a NEW file in /cer/fs and opens it for writing, returning the
// data-plane endpoint descriptor the caller sends the bytes to — the same grant
// Open's write branch mints for an existing file, reached through the verb 9P
// actually defines for creation.
//
// This exists because a walk to a path that holds no file is now ENOENT (see
// GetAttr): p9's doWalk GetAttrs every component it walks, so "walk to a name
// that does not exist yet, then open it for write" — the shortcut that used to
// create a file over the wire — cannot survive an honest stat. That shortcut was
// never how a filesystem creates a file; O_CREAT is, and it arrives here as
// Tlcreate. So creation keeps working over 9P, through the right door, and
// phantom paths still stat as absent.
//
// It is refused anywhere but a /cer/fs directory: on a device directory or a
// leaf the embedded templatefs.NotDirectoryFile's ENOTDIR still stands, so this
// adds no way to create anything outside the filesystem subtree. The capability
// check is OpenFSWrite's ("write" on the new file's own fsResource) — creating a
// file is exactly as authorized as writing one.
func (n *node) Create(name string, mode p9.OpenFlags, _ p9.FileMode, _ p9.UID, _ p9.GID) (p9.File, p9.QID, uint32, error) {
	if !n.dir || !isFSPath(n.path) {
		return nil, p9.QID{}, 0, linux.ENOTDIR
	}
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return nil, p9.QID{}, 0, linux.EINVAL
	}
	child := &node{ns: n.ns, caps: n.caps, path: joinPath(n.path, name), fsFile: true}
	ep, err := tryVal(n.caps, func(c contract.CapHandle) (DataEndpoint, error) { return n.ns.OpenFSWrite(child.path, c) })
	if err != nil {
		return nil, p9.QID{}, 0, toErrno(err)
	}
	b, err := json.Marshal(ep)
	if err != nil {
		return nil, p9.QID{}, 0, linux.EIO
	}
	child.data = b
	return child, child.qid(), 0, nil
}

// ReadAt serves either a read-opened /cer/fs file's bytes (ranged, straight out
// of the dfs engine — only the chunks this range touches are reconstructed) or
// the descriptor bytes captured at Open (endpoint JSON for ctl, info JSON for
// info). It never reads from a device: a device ctl still yields only its
// endpoint descriptor.
func (n *node) ReadAt(p []byte, off int64) (int, error) {
	if n.fsRead != nil {
		return n.fsRead.ReadAt(p, off)
	}
	if off < 0 || off >= int64(len(n.data)) {
		return 0, io.EOF
	}
	c := copy(p, n.data[off:])
	if off+int64(c) >= int64(len(n.data)) {
		return c, io.EOF
	}
	return c, nil
}

// --- helpers --------------------------------------------------------------

func isLeaf(name string) bool { return name == "ctl" || name == "info" || name == "alloc" }

func joinPath(dir, name string) string {
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

func basePath(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func fnv64(s string) uint64 {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// toErrno maps a contract.CapError onto a 9P/Linux errno so the wire client sees
// a denial, not bytes. DENIED/REVOKED/QUOTA_EXCEEDED surface as EACCES; an
// unknown path or a /cer/fs path that holds no file surfaces as ENOENT.
func toErrno(err error) error {
	var ce *contract.CapError
	if errors.As(err, &ce) {
		switch ce.Code {
		case contract.ErrDenied:
			// "no such file" is what the /cer/fs store reports for a path that
			// was never written; it is an absence, not a denial, and a shell
			// must see ENOENT for it or `ls` prints "permission denied" for a
			// file that simply is not there.
			if strings.Contains(ce.Msg, "no such path") || strings.Contains(ce.Msg, "no such file") {
				return linux.ENOENT
			}
			return linux.EACCES
		case contract.ErrRevoked, contract.ErrQuotaExceeded:
			return linux.EACCES
		}
	}
	return linux.EACCES
}
