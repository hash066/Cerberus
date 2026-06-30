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
//	by an Attacher pinned to exactly one CapHandle. This realises vertical 04 §7
//	("the namespace is per-principal: a node only sees device files for which it
//	holds a capability") — one connection == one principal == one capability
//	view. EVERY Twalk/Topen on that connection is still checked through the
//	existing Server against that connection's capability; there is no ambient
//	authority. A future networked transport would, at connection-accept time,
//	exchange a signed CBOR capability (schemas/capability.cddl) and translate it
//	into a local CapHandle before constructing the Attacher — the seam is the
//	same.
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
	srv := p9.NewServer(&attacher{ns: w.ns, cap: cap})
	return srv.Serve(l)
}

// Handle serves a single 9P connection over the given read/write pipe, bound to
// cap. Used by DialCap for the in-process loopback transport and usable by any
// caller that already has an accepted connection.
func (w *WireServer) Handle(r io.ReadCloser, wr io.WriteCloser, cap contract.CapHandle) error {
	srv := p9.NewServer(&attacher{ns: w.ns, cap: cap})
	return srv.Handle(r, wr)
}

// DialCap starts an in-process 9P2000.L server for ns over an in-memory socket
// pair, bound to cap, and returns a connected 9P client attached at the root.
// This is the loopback path used by tests and by same-host components; the wire
// format on the pipe is the real 9P2000.L encoding, not a shortcut.
func DialCap(ns *Server, cap contract.CapHandle) (*p9.Client, func() error, error) {
	c1, c2 := net.Pipe()
	w := NewWireServer(ns)
	go func() { _ = w.Handle(c2, c2, cap) }()
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

// attacher binds one connection to one capability.
type attacher struct {
	ns  *Server
	cap contract.CapHandle
}

// Attach returns the namespace root for this connection's capability.
func (a *attacher) Attach() (p9.File, error) {
	return &node{ns: a.ns, cap: a.cap, path: rootPath, dir: true}, nil
}

// node is one path in the namespace. dir distinguishes a directory (walkable,
// not byte-readable) from a leaf (ctl/info). data is set on a leaf after Open so
// ReadAt can serve the descriptor bytes (endpoint JSON for ctl, info JSON for
// info) — never device bytes; the device data plane is the QUIC/RDMA endpoint
// the ctl descriptor points at.
type node struct {
	templatefs.ReadOnlyFile
	templatefs.NilCloser
	templatefs.NotLockable

	ns   *Server
	cap  contract.CapHandle
	path string
	dir  bool
	data []byte // populated on Open for openable leaves
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
	return p9.ModeRegular | 0o444
}

// GetAttr reports node attributes. Required by Attach (it calls GetAttr) and by
// clients before open.
func (n *node) GetAttr(req p9.AttrMask) (p9.QID, p9.AttrMask, p9.Attr, error) {
	attr := p9.Attr{Mode: n.mode(), Size: uint64(len(n.data)), NLink: 1}
	mask := p9.AttrMask{Mode: true, NLink: true, Size: true}
	return n.qid(), mask, attr, nil
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
		return []p9.QID{}, &clone, nil
	}
	cur := n
	qids := make([]p9.QID, 0, len(names))
	for _, name := range names {
		if name == "" || name == "." || strings.Contains(name, "/") {
			return nil, nil, linux.EINVAL
		}
		next := &node{ns: n.ns, cap: n.cap, path: joinPath(cur.path, name)}
		leaf := isLeaf(name)
		next.dir = !leaf
		switch {
		case leaf:
			// ctl/info/alloc are leaves; the parent must be a registered device
			// and the cap must authorize read on it (Server.Walk = "read"
			// check) — so a capability is required even to SEE the leaf. The
			// stronger alloc/read gate for the actual endpoint/bytes fires at
			// Open.
			if err := n.ns.Walk(cur.path, n.cap); err != nil {
				return nil, nil, toErrno(err)
			}
		case n.ns.IsAncestorDir(next.path):
			// Structural ancestor of some device (e.g. /cer/dev/vram): no
			// resource of its own, traversable without a capability, exposes no
			// bytes. The cap check still fires once we reach the device itself.
		default:
			// A registered device directory (or unknown path): capability-check
			// read through the existing Server. Unknown paths are denied here.
			if err := n.ns.Walk(next.path, n.cap); err != nil {
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
	switch basePath(n.path) {
	case "ctl":
		ep, err := n.ns.Open(n.path, n.cap)
		if err != nil {
			return p9.QID{}, 0, toErrno(err)
		}
		b, err := json.Marshal(ep)
		if err != nil {
			return p9.QID{}, 0, linux.EIO
		}
		n.data = b
	case "info":
		b, err := n.ns.ReadInfo(n.path, n.cap)
		if err != nil {
			return p9.QID{}, 0, toErrno(err)
		}
		n.data = b
	default:
		return p9.QID{}, 0, linux.EACCES
	}
	return n.qid(), 0, nil
}

// ReadAt serves the descriptor bytes captured at Open (endpoint JSON for ctl,
// info JSON for info). It never reads from a device.
func (n *node) ReadAt(p []byte, off int64) (int, error) {
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
// unknown path surfaces as ENOENT.
func toErrno(err error) error {
	var ce *contract.CapError
	if errors.As(err, &ce) {
		switch ce.Code {
		case contract.ErrDenied:
			if strings.Contains(ce.Msg, "no such path") {
				return linux.ENOENT
			}
			return linux.EACCES
		case contract.ErrRevoked, contract.ErrQuotaExceeded:
			return linux.EACCES
		}
	}
	return linux.EACCES
}
