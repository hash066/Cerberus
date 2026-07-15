//go:build linux

// Real FUSE mount of the 9P capability namespace on Linux, using
// github.com/hanwen/go-fuse — the pure-Go FUSE library.
//
// WHY go-fuse AND NOT cgofuse HERE. cgofuse's Unix binding needs libfuse and
// therefore cgo, which would break this repo's default CGO_ENABLED=0 build (see
// CLAUDE.md; the same constraint mount_windows.go navigates on the Windows side
// by using cgofuse's nocgo WinFsp variant). hanwen/go-fuse instead speaks the
// FUSE wire protocol to /dev/fuse directly, in Go: `CGO_ENABLED=0 go list` for
// github.com/hanwen/go-fuse/v2/fs and .../fuse both report an EMPTY CgoFiles
// list, so this file adds a real Linux mount with no C toolchain, no libfuse,
// and no cgo — the mount and the Rust-kernel (`-tags ffi`) builds stay
// independent instead of mutually exclusive.
//
// Unlike Windows/WinFsp, Linux needs no third-party driver install: fuse is in
// the mainline kernel. The only host requirements are that /dev/fuse exists and
// is openable, and that the mountpoint is an existing directory — both are
// reported as specific, actionable errors below when absent.
//
// THE CAPABILITY INVARIANT (CLAUDE.md golden rule 5): exactly as on Windows,
// every callback below (Lookup/Getattr/Open/Read/Readdir/Write) is implemented
// by walking/opening a real 9P2000.L client connection obtained from
// DialCap(ns, cap) — the same entry point wire.go's WireServer uses for network
// peers, and the same capability-checked logic (Server.Walk/Open/ReadInfo/
// WalkFS/OpenFSWrite, all gated by kernel.Verify). There is no second,
// unguarded path from the mountpoint into the namespace: a mount bound to
// capability X sees and opens exactly what a 9P client bound to X sees.
package ninep

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hugelgupf/p9/linux"
	"github.com/hugelgupf/p9/p9"

	contract "github.com/hash066/cerberus/contract/go"
)

// mount is the Linux entry point for Mount (see mount.go). It hosts a go-fuse
// server backed by cfg.NS, scoped to cfg.Cap, at cfg.Mountpoint.
//
// Like the Windows implementation, this does not block: go-fuse's dispatch loop
// runs on a background goroutine (server.Wait) and mount returns as soon as the
// filesystem is live, so the caller keeps control.
func mount(cfg MountConfig) error {
	if cfg.NS == nil {
		return contract.Errf(contract.ErrDenied, "mount: MountConfig.NS (the namespace to present) must not be nil")
	}
	if strings.TrimSpace(cfg.Mountpoint) == "" {
		return contract.Errf(contract.ErrDenied, "mount: MountConfig.Mountpoint must not be empty")
	}
	// FUSE mounts OVER an existing directory. Check it up front so the operator
	// gets "make this directory" rather than a bare mount syscall errno.
	if st, err := os.Stat(cfg.Mountpoint); err != nil {
		return contract.Errf(contract.ErrDenied,
			"mount: mountpoint "+cfg.Mountpoint+" is not usable: "+err.Error()+
				" — a Linux FUSE mount needs an EXISTING directory to mount over (mkdir -p "+cfg.Mountpoint+")")
	} else if !st.IsDir() {
		return contract.Errf(contract.ErrDenied, "mount: mountpoint "+cfg.Mountpoint+" exists but is not a directory")
	}

	cl, closer, err := DialCap(cfg.NS, cfg.Cap)
	if err != nil {
		return contract.Errf(contract.ErrPartitioned, "mount: dialing the in-process 9P namespace failed: "+err.Error())
	}
	root, err := cl.Attach("/")
	if err != nil {
		_ = closer()
		return contract.Errf(contract.ErrDenied, "mount: attach to /cer with the given capability failed: "+err.Error())
	}

	m := &linuxMount{ns: cfg.NS, cap: cfg.Cap, root: root, client: cl, closer: closer, mountpoint: cfg.Mountpoint}
	rootNode := &p9node{m: m, path: "", isDir: true}

	server, err := fs.Mount(cfg.Mountpoint, rootNode, &fs.Options{
		MountOptions: fuse.MountOptions{
			Name:   "cerberus",
			FsName: "cerberus",
			// The namespace is a control plane, not a data store: it is served
			// read-mostly and its contents change only when a device is
			// registered, so letting the kernel cache attrs/entries is safe and
			// avoids a 9P round trip per stat. Left at go-fuse's 1s default by
			// passing no explicit timeout.
			//
			// DirectMount makes go-fuse mount via the mount(2) syscall instead
			// of shelling out to fusermount. It is what lets this work on hosts
			// that have /dev/fuse but no fuse-utils package installed (e.g. a
			// stock WSL2 Debian, where this was verified), and go-fuse falls
			// back to fusermount automatically when the direct syscall is not
			// permitted (i.e. unprivileged users on a normal distro keep
			// working through the setuid helper).
			DirectMount: true,
		},
	})
	if err != nil {
		_ = closer()
		return mountSetupError(cfg.Mountpoint, err)
	}

	m.server = server
	if err := registerMount(cfg.Mountpoint, m); err != nil {
		_ = server.Unmount()
		_ = closer()
		return err
	}

	// go-fuse's dispatch loop. fs.Mount has already completed the mount
	// handshake by the time it returns, so the filesystem is live now; Wait
	// simply blocks until it is unmounted.
	go server.Wait()
	return nil
}

// unmount is the Linux entry point for Unmount (see mount.go).
func unmount(cfg MountConfig) error {
	return unmountRegistered(cfg.Mountpoint)
}

// mountSetupError turns a go-fuse mount failure into a specific, actionable
// *contract.CapError. The dominant real-world causes are a missing/unopenable
// /dev/fuse (no fuse kernel module, or a container without the device) and a
// permission denial (unprivileged user on a host with no fusermount helper) —
// name them explicitly rather than surfacing a bare errno, matching the WinFsp
// side's install-hint posture (CLAUDE.md "Maturity honesty").
func mountSetupError(mountpoint string, err error) error {
	msg := err.Error()
	if _, statErr := os.Stat("/dev/fuse"); statErr != nil {
		return contract.Errf(contract.ErrPartitioned,
			"mount: /dev/fuse is not present on this host ("+statErr.Error()+"), so no FUSE filesystem can be mounted — "+
				"load the fuse kernel module (modprobe fuse), or on a container run it with --device /dev/fuse. Underlying error: "+msg)
	}
	if os.IsPermission(err) || strings.Contains(msg, "permission denied") || strings.Contains(msg, "operation not permitted") {
		return contract.Errf(contract.ErrDenied,
			"mount: not permitted to mount at "+mountpoint+" — /dev/fuse exists but this process may not mount over it. "+
				"Either install fuse's setuid helper (apt install fuse3) so an unprivileged user can mount, "+
				"or run with CAP_SYS_ADMIN/root. Underlying error: "+msg)
	}
	return contract.Errf(contract.ErrPartitioned, "mount: go-fuse could not mount at "+mountpoint+": "+msg)
}

// linuxMount is one live Linux mount: the go-fuse server plus the single 9P
// client connection (bound to exactly one capability by DialCap) that every
// callback is served from. It implements mount.go's mountHandle.
type linuxMount struct {
	ns         *Server            // used only by Readdir, via the cap-gated ListChildren
	cap        contract.CapHandle // the single capability this mount is bound to
	root       p9.File            // the /cer root, attached once for the life of the mount
	client     *p9.Client
	closer     func() error
	mountpoint string
	server     *fuse.Server
}

// unmount detaches the go-fuse server and releases the 9P client behind it.
func (m *linuxMount) unmount() error {
	if err := m.server.Unmount(); err != nil {
		return contract.Errf(contract.ErrPartitioned, "unmount: go-fuse could not unmount "+m.mountpoint+": "+err.Error())
	}
	_ = m.closer()
	return nil
}

// walk resolves a mount-relative path against the namespace root over the live
// 9P client connection — the same capability-checked Walk wire.go's node.Walk
// performs for a network peer. Returns the resolved file and whether the final
// component was reported as a directory by the wire QID type.
func (m *linuxMount) walk(p string) (p9.File, bool, error) {
	names := components(p)
	if len(names) == 0 {
		// Walk-to-self: clones the root fid, mirroring wire.go's node.Walk
		// empty-names branch.
		_, f, err := m.root.Walk(nil)
		if err != nil {
			return nil, false, err
		}
		return f, true, nil
	}
	qids, f, err := m.root.Walk(names)
	if err != nil {
		return nil, false, err
	}
	isDir := len(qids) > 0 && qids[len(qids)-1].Type == p9.TypeDir
	return f, isDir, nil
}

// --- node ------------------------------------------------------------------

// p9node is one path in the mounted namespace. It holds no authority of its
// own: every operation goes back through m's capability-bound 9P client.
type p9node struct {
	fs.Inode

	m     *linuxMount
	path  string // mount-relative ("" is the /cer root)
	isDir bool
}

// Compile-time proof that p9node implements exactly the FUSE operations this
// namespace supports — and, by omission, that it implements none of the
// mutating ones (Mkdir/Rmdir/Unlink/Rename/Setattr), which therefore return
// ENOSYS from go-fuse's defaults rather than being silently faked.
var (
	_ fs.InodeEmbedder  = (*p9node)(nil)
	_ fs.NodeLookuper   = (*p9node)(nil)
	_ fs.NodeReaddirer  = (*p9node)(nil)
	_ fs.NodeGetattrer  = (*p9node)(nil)
	_ fs.NodeOpener     = (*p9node)(nil)
	_ fs.NodeReader     = (*p9node)(nil)
	_ fs.NodeWriter     = (*p9node)(nil)
	_ fs.FileReleaser   = (*p9handle)(nil)
)

func (n *p9node) childPath(name string) string {
	if n.path == "" {
		return name
	}
	return n.path + "/" + name
}

// Lookup resolves one name below this node through the capability-checked 9P
// Walk. A name the capability does not authorize fails here exactly as it would
// for a 9P client — the mount never invents an entry.
func (n *p9node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	child := n.childPath(name)
	f, isDir, err := n.m.walk(child)
	if err != nil {
		return nil, toLinuxErrno(err)
	}
	defer f.Close()

	_, _, attr, aerr := f.GetAttr(p9.AttrMaskAll)
	if aerr != nil {
		return nil, toLinuxErrno(aerr)
	}
	fillAttr(&out.Attr, isDir, attr)

	mode := uint32(syscall.S_IFREG)
	if isDir {
		mode = syscall.S_IFDIR
	}
	// Ino is derived from the namespace path with the same FNV hash wire.go's
	// qid() uses, so a path has one stable inode number for the mount's life.
	stable := fs.StableAttr{Mode: mode, Ino: fnv64(nsPath(child))}
	return n.NewInode(ctx, &p9node{m: n.m, path: child, isDir: isDir}, stable), 0
}

// Readdir lists this directory's children via Server.ListChildren — the
// capability-filtered enumeration helper (see ninep.go) that applies the
// identical check Walk performs per entry, so a listing can never name an entry
// the capability could not walk to.
func (n *p9node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	names := n.m.ns.ListChildren(nsPath(n.path), n.m.cap)
	entries := make([]fuse.DirEntry, 0, len(names))
	for _, name := range names {
		// ctl/info/alloc are the namespace's only leaves; everything else a
		// listing yields is a directory (structural ancestor, device dir, or
		// the /cer/fs root) — the same classification wire.go's node.Walk makes
		// via isLeaf.
		mode := uint32(syscall.S_IFDIR)
		if isLeaf(name) {
			mode = syscall.S_IFREG
		}
		entries = append(entries, fuse.DirEntry{Name: name, Mode: mode, Ino: fnv64(nsPath(n.childPath(name)))})
	}
	return fs.NewListDirStream(entries), 0
}

// Getattr re-walks (or re-queries the open handle) through the capability-checked
// 9P client. There is no attribute cache here that could outlive a revocation;
// the kernel's own 1s entry/attr cache is the only staleness window.
func (n *p9node) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if h, ok := f.(*p9handle); ok && h != nil {
		_, _, attr, err := h.file.GetAttr(p9.AttrMaskAll)
		if err != nil {
			return toLinuxErrno(err)
		}
		fillAttr(&out.Attr, n.isDir, attr)
		return 0
	}
	file, isDir, err := n.m.walk(n.path)
	if err != nil {
		return toLinuxErrno(err)
	}
	defer file.Close()
	_, _, attr, aerr := file.GetAttr(p9.AttrMaskAll)
	if aerr != nil {
		return toLinuxErrno(aerr)
	}
	fillAttr(&out.Attr, isDir, attr)
	return 0
}

// Open opens a leaf. Only the namespace's openable leaves (.../ctl, .../info,
// and /cer/fs/<path> files) succeed — the 9P server itself refuses every other
// path before this code sees a result, so there is nothing to relax here.
func (n *p9node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	f, isDir, err := n.m.walk(n.path)
	if err != nil {
		return nil, 0, toLinuxErrno(err)
	}
	if isDir {
		f.Close()
		return nil, 0, syscall.EISDIR
	}
	mode := p9.ReadOnly
	switch flags & uint32(syscall.O_ACCMODE) {
	case uint32(syscall.O_WRONLY):
		mode = p9.WriteOnly
	case uint32(syscall.O_RDWR):
		mode = p9.ReadWrite
	}
	if _, _, err := f.Open(mode); err != nil {
		f.Close()
		return nil, 0, toLinuxErrno(err)
	}
	return &p9handle{file: f}, 0, 0
}

// Read serves the descriptor bytes captured at Open (the ctl/info/fs-write
// DataEndpoint JSON, or the info JSON) — never device bytes, upholding the
// vertical 04 §3.5 invariant end to end through the mount.
func (n *p9node) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h, ok := f.(*p9handle)
	if !ok {
		return nil, syscall.EBADF
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.data == nil {
		data, err := readAllFrom(h.file)
		if err != nil && len(data) == 0 {
			return nil, toLinuxErrno(err)
		}
		h.data = data
	}
	if off < 0 || off >= int64(len(h.data)) {
		return fuse.ReadResultData(nil), 0
	}
	return fuse.ReadResultData(h.data[off:]), 0
}

// Write forwards to the 9P client's WriteAt, which server-side is only
// reachable on a /cer/fs write-open node (wire.go's node.Open dispatches such
// writes to Server.OpenFSWrite; a device ctl/info leaf is refused server-side
// already). It adds no authority check of its own because the one real
// authority (Server.OpenFSWrite) already ran during Open.
func (n *p9node) Write(ctx context.Context, f fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	h, ok := f.(*p9handle)
	if !ok {
		return 0, syscall.EBADF
	}
	written, err := h.file.WriteAt(data, off)
	if err != nil {
		return 0, toLinuxErrno(err)
	}
	return uint32(written), 0
}

// p9handle is what a FUSE file handle refers to: the walked+opened p9.File plus
// its descriptor bytes, captured lazily on first Read.
type p9handle struct {
	mu   sync.Mutex
	file p9.File
	data []byte
}

// Release closes the underlying 9P file when the kernel drops the handle.
func (h *p9handle) Release(ctx context.Context) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file != nil {
		_ = h.file.Close()
		h.file = nil
	}
	return 0
}

// fillAttr maps a 9P Attr onto a FUSE attr. The namespace is read-mostly: a
// directory is r-x, a leaf is r-- unless the 9P server reported it writable
// (which today means a /cer/fs file — see wire.go's node.mode).
func fillAttr(out *fuse.Attr, isDir bool, attr p9.Attr) {
	if isDir {
		out.Mode = syscall.S_IFDIR | 0o555
	} else {
		out.Mode = syscall.S_IFREG | 0o444
		if attr.Mode&0o200 != 0 {
			out.Mode = syscall.S_IFREG | 0o644
		}
	}
	out.Nlink = 1
	out.Size = attr.Size
}

// toLinuxErrno maps a 9P client error onto a syscall.Errno. wire.go's toErrno
// already turns every contract.CapError into a linux.Errno at the SERVER before
// it reaches the wire, so in practice every error observed here is already a
// linux.Errno; the CapError branch is defensive belt-and-braces for the
// in-process DialCap path only.
//
// This maps by name via an explicit switch rather than by numeric coincidence,
// so a future change to hugelgupf/p9's linux.Errno values cannot silently
// miswire an errno — the same reasoning as mount_windows.go's toFuseErrno.
func toLinuxErrno(err error) syscall.Errno {
	if err == nil || err == io.EOF {
		return 0
	}
	if ce, ok := err.(*contract.CapError); ok {
		switch ce.Code {
		case contract.ErrDenied:
			if strings.Contains(ce.Msg, "no such path") {
				return syscall.ENOENT
			}
			return syscall.EACCES
		case contract.ErrRevoked, contract.ErrQuotaExceeded:
			return syscall.EACCES
		case contract.ErrPartitioned:
			return syscall.EIO
		}
		return syscall.EACCES
	}
	switch err {
	case linux.ENOENT:
		return syscall.ENOENT
	case linux.EACCES:
		return syscall.EACCES
	case linux.EISDIR:
		return syscall.EISDIR
	case linux.ENOTDIR:
		return syscall.ENOTDIR
	case linux.ENOSYS:
		return syscall.ENOSYS
	case linux.EINVAL:
		return syscall.EINVAL
	case linux.EBADF:
		return syscall.EBADF
	}
	// Unrecognized error: fail closed with I/O error rather than guessing, so an
	// unmapped denial can never be misreported as success.
	return syscall.EIO
}
