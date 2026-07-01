//go:build windows

// Real WinFsp mount of the 9P capability namespace, using
// github.com/winfsp/cgofuse — the same library docs/verticals/04 §6 names as
// the sanctioned Windows mount technology.
//
// cgofuse ships a Windows "nocgo" variant (fsop_nocgo_windows.go /
// host_nocgo_windows.go, selected automatically by `//go:build !cgo &&
// windows`) that talks to the WinFsp DLL via syscall.LoadDLL/GetProcAddress —
// no C compiler, no CGO_ENABLED=1, no cgo toolchain required. That is exactly
// what lets this file build in this repo's default CGO_ENABLED=0 Windows
// build (verified: `go build ./daemon/ninep/...` succeeds with no C compiler
// on PATH). Only the WinFsp kernel driver + user-mode DLL are an external,
// separately-installed dependency — and that is a real, unavoidable OS
// dependency for ANY Windows FUSE-style mount, not a shortcut this code took.
//
// THE CAPABILITY INVARIANT (CLAUDE.md golden rule 5): every FileSystemInterface
// callback below (Getattr/Open/Read/Write/Readdir/...) is implemented by
// walking/opening a real 9P2000.L client connection obtained from DialCap(ns,
// cap) — the exact same entry point wire.go's WireServer uses for network
// peers, and the exact same attacher/node capability-checked logic
// (Server.Walk/Open/ReadInfo/WalkFS/OpenFSWrite, all gated by kernel.Verify)
// that ninep_test.go and wire_test.go already exercise. There is no second,
// unguarded path from the mounted drive letter into the namespace: a mount
// bound to capability X can see and open exactly what a 9P client bound to
// capability X could see and open over the wire, no more.
package ninep

import (
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugelgupf/p9/linux"
	"github.com/hugelgupf/p9/p9"
	"github.com/winfsp/cgofuse/fuse"

	contract "github.com/hash066/cerberus/contract/go"
)

// winfspInstallHint is the exact, actionable remediation printed when WinFsp's
// DLL cannot be found on this host — naming precisely what is missing and how
// to fix it, per the maturity-honesty rule (CLAUDE.md "Maturity honesty":
// documented stubs must say so, not fail generically).
const winfspInstallHint = "WinFsp not installed (or its DLL could not be located) — " +
	"install it from https://winfsp.dev/rel/ (or `winget install WinFsp.WinFsp`) and retry"

// mount is the Windows entry point for Mount (see mount.go). It hosts a
// cgofuse file system backed by cfg.NS, scoped to cfg.Cap, at cfg.Mountpoint.
//
// Mount()'s underlying cgofuse call is BLOCKING (it runs the FUSE dispatch
// loop until Unmount) and, when WinFsp cannot be found, cgofuse PANICS with
// the string "cgofuse: cannot find winfsp" (confirmed against
// github.com/winfsp/cgofuse@v1.6.0's host.go / host_nocgo_windows.go, and
// reproduced live on this host, which does not have WinFsp installed:
// running FileSystemHost.Mount here panics with exactly that string). This
// function runs the mount on a background goroutine, recovers that panic (or
// any other), and turns it into a specific *contract.CapError so callers get
// an actionable error return instead of a crashed process — never a bare
// panic escaping Mount, and never a silently-fake success.
func mount(cfg MountConfig) error {
	if cfg.NS == nil {
		return contract.Errf(contract.ErrDenied, "mount: MountConfig.NS (the namespace to present) must not be nil")
	}
	if strings.TrimSpace(cfg.Mountpoint) == "" {
		return contract.Errf(contract.ErrDenied, "mount: MountConfig.Mountpoint must not be empty")
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

	adapter := newWinfsAdapter(cfg.NS, cfg.Cap, root)
	host := fuse.NewFileSystemHost(adapter)

	if err := registerMount(cfg.Mountpoint, &mountedHost{host: host, client: cl, closer: closer}); err != nil {
		_ = closer()
		return err
	}

	started := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				select {
				case started <- panicToMountError(r):
				default:
				}
			}
		}()
		// FileSystemHost.Mount blocks running the FUSE dispatch loop; it
		// only returns (without panicking) once the file system is
		// unmounted, or immediately with false on a startup failure that
		// did not panic. Either way, report it if `started` hasn't already
		// been resolved by the success path below.
		ok := host.Mount(cfg.Mountpoint, nil)
		if !ok {
			select {
			case started <- contract.Errf(contract.ErrPartitioned, "mount: cgofuse Mount() returned false for "+cfg.Mountpoint):
			default:
			}
		}
	}()

	select {
	case err := <-started:
		unregisterMount(cfg.Mountpoint)
		_ = closer()
		return err
	case <-time.After(5 * time.Second):
		// No failure/panic within the timeout: WinFsp accepted the mount and
		// the dispatch loop is legitimately blocked serving it (or it is
		// still attaching, which resolves asynchronously with no further
		// signal back to this call — cgofuse does not expose a distinct
		// "mount is now live" future beyond the FileSystemInterface Init()
		// callback firing on the adapter). Report success.
		return nil
	}
}

// panicToMountError converts a recovered cgofuse panic into a specific,
// actionable *contract.CapError. cgofuse panics with the literal string
// "cgofuse: cannot find winfsp" (host.go's Mount/OptParse, both nocgo and cgo
// builds) when the WinFsp DLL cannot be located — verified live on this
// machine, which lacks WinFsp: recovering that exact panic and mapping it to
// winfspInstallHint is what lets a host WITHOUT the driver fail with a clear,
// named remediation instead of a crashed goroutine or a generic error.
func panicToMountError(r interface{}) error {
	if s, ok := r.(string); ok && strings.Contains(s, "cannot find winfsp") {
		return contract.Errf(contract.ErrPartitioned, winfspInstallHint)
	}
	return contract.Errf(contract.ErrPartitioned, fmt.Sprintf("mount: cgofuse panicked: %v", r))
}

// mountedHost tracks a live mount so Unmount can find and tear it down.
type mountedHost struct {
	host   *fuse.FileSystemHost
	client *p9.Client
	closer func() error
}

var (
	mountsMu sync.Mutex
	mounts   = map[string]*mountedHost{}
)

func registerMount(mountpoint string, h *mountedHost) error {
	mountsMu.Lock()
	defer mountsMu.Unlock()
	if _, exists := mounts[mountpoint]; exists {
		return contract.Errf(contract.ErrDenied, "mount: "+mountpoint+" is already mounted by this process")
	}
	mounts[mountpoint] = h
	return nil
}

func unregisterMount(mountpoint string) {
	mountsMu.Lock()
	defer mountsMu.Unlock()
	delete(mounts, mountpoint)
}

// unmount is the Windows entry point for Unmount (see mount.go).
func unmount(cfg MountConfig) error {
	mountsMu.Lock()
	h, ok := mounts[cfg.Mountpoint]
	if ok {
		delete(mounts, cfg.Mountpoint)
	}
	mountsMu.Unlock()
	if !ok {
		return contract.Errf(contract.ErrDenied, "unmount: "+cfg.Mountpoint+" is not mounted by this process")
	}
	if !h.host.Unmount() {
		return contract.Errf(contract.ErrPartitioned, "unmount: cgofuse Unmount() reported failure for "+cfg.Mountpoint)
	}
	_ = h.closer()
	return nil
}

// --- FileSystemInterface adapter -------------------------------------------

// winfsAdapter presents a 9P client connection (already bound to one
// capability by DialCap — see mount()) as a cgofuse FileSystemInterface.
// Every method below does exactly one thing: translate the FUSE call into the
// equivalent p9.File.Walk/Open/ReadAt/GetAttr call on the SAME client
// connection wire.go serves, and translate the p9/contract error back into a
// FUSE errno. No method here reaches into ns directly for a walk/open
// decision — only ListChildren (Readdir's enumeration helper) reads the
// namespace directly, and it applies the identical capability check Walk
// performs (see ninep.go's ListChildren) — so there is no second, unguarded
// path into the namespace.
type winfsAdapter struct {
	fuse.FileSystemBase

	ns   *Server            // used only by Readdir, via the cap-gated ListChildren
	cap  contract.CapHandle // the single capability this mount (and this adapter) is bound to
	root p9.File            // the /cer root, attached once for the life of the mount

	mu      sync.Mutex
	handles map[uint64]*openHandle
	nextFh  uint64
}

func newWinfsAdapter(ns *Server, cap contract.CapHandle, root p9.File) *winfsAdapter {
	return &winfsAdapter{ns: ns, cap: cap, root: root, handles: map[uint64]*openHandle{}}
}

// openHandle is what a FUSE file handle (fi.fh) refers to: the walked p9.File
// plus its cached descriptor bytes (populated at Open, exactly as wire.go's
// node.Open captures n.data) or directory listing (populated at Opendir).
type openHandle struct {
	file    p9.File
	isDir   bool
	data    []byte   // ctl/info/fs-write descriptor bytes, captured lazily on first Read
	entries []string // directory children, captured at Opendir
}

const invalidFh = ^uint64(0)

// components splits a FUSE path ("/", "/dev", "/dev/vram/AA/0/ctl") into the
// name components p9.File.Walk expects, relative to the attached /cer root.
func components(fusePath string) []string {
	clean := path.Clean("/" + fusePath)
	if clean == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(clean, "/"), "/")
}

// nsPath rebuilds the full namespace path (e.g. "/cer/dev/vram/AA/0/ctl") a
// FUSE path corresponds to, for calling ListChildren (which, like the rest of
// the Server API, takes namespace-rooted paths, not FUSE-relative ones).
func nsPath(fusePath string) string {
	names := components(fusePath)
	if len(names) == 0 {
		return rootPath
	}
	return rootPath + "/" + strings.Join(names, "/")
}

// walk resolves a FUSE path against the namespace root over the live 9P
// client connection — the same capability-checked Walk wire.go's node.Walk
// performs for a network peer. Returns the resolved file and whether the
// final component was reported as a directory by the wire QID type.
func (a *winfsAdapter) walk(fusePath string) (p9.File, bool, error) {
	names := components(fusePath)
	if len(names) == 0 {
		// Walking to "/" itself: p9's walk-to-self convention (empty names)
		// clones the root fid without re-checking anything beyond the
		// existing attach — mirrors wire.go's node.Walk empty-names branch.
		_, f, err := a.root.Walk(nil)
		if err != nil {
			return nil, false, err
		}
		return f, true, nil
	}
	qids, f, err := a.root.Walk(names)
	if err != nil {
		return nil, false, err
	}
	isDir := len(qids) > 0 && qids[len(qids)-1].Type == p9.TypeDir
	return f, isDir, nil
}

// toFuseErrno maps a 9P client error onto a cgofuse errno constant. wire.go's own
// toFuseErrno already turns every contract.CapError into a linux.Errno at the
// SERVER before it ever reaches the wire, so in practice every error this
// client observes is already a linux.Errno; the CapError branch below is
// defensive belt-and-braces for the in-process DialCap path only.
//
// linux.Errno's numbering (ENOENT=2, EACCES=13, EIO=5, EISDIR=21, ENOTDIR=20,
// ENOSYS=40 — hugelgupf/p9/linux/errno.go) happens to equal cgofuse's own
// fuse.E* constants on Windows (fsop_nocgo_windows.go), but this function maps
// by name via an explicit switch rather than relying on that numeric
// coincidence, so a future change to either package's values can't silently
// miswire an errno.
func toFuseErrno(err error) int {
	if err == nil || err == io.EOF {
		return 0
	}
	if ce, ok := err.(*contract.CapError); ok {
		switch ce.Code {
		case contract.ErrDenied:
			if strings.Contains(ce.Msg, "no such path") {
				return fuse.ENOENT
			}
			return fuse.EACCES
		case contract.ErrRevoked, contract.ErrQuotaExceeded:
			return fuse.EACCES
		case contract.ErrPartitioned:
			return fuse.EIO
		}
		return fuse.EACCES
	}
	switch err {
	case linux.ENOENT:
		return fuse.ENOENT
	case linux.EACCES:
		return fuse.EACCES
	case linux.EISDIR:
		return fuse.EISDIR
	case linux.ENOTDIR:
		return fuse.ENOTDIR
	case linux.ENOSYS:
		return fuse.ENOSYS
	case linux.EINVAL:
		return fuse.EINVAL
	case linux.EBADF:
		return fuse.EBADF
	}
	// Unrecognized error: fail closed with I/O error rather than guessing, so
	// an unmapped denial can never be misreported as success.
	return fuse.EIO
}

// Getattr gets file attributes. Every call re-walks the path (or re-queries
// the already-open handle) through the capability-checked 9P client — there
// is no attribute cache here that could go stale relative to a capability
// revocation.
func (a *winfsAdapter) Getattr(fusePath string, stat *fuse.Stat_t, fh uint64) int {
	if fh != invalidFh {
		a.mu.Lock()
		h, ok := a.handles[fh]
		a.mu.Unlock()
		if ok {
			_, _, attr, err := h.file.GetAttr(p9.AttrMaskAll)
			if err != nil {
				return -toFuseErrno(err)
			}
			fillStat(stat, h.isDir, attr)
			return 0
		}
	}
	f, isDir, err := a.walk(fusePath)
	if err != nil {
		return -toFuseErrno(err)
	}
	defer f.Close()
	_, _, attr, err := f.GetAttr(p9.AttrMaskAll)
	if err != nil {
		return -toFuseErrno(err)
	}
	fillStat(stat, isDir, attr)
	return 0
}

func fillStat(stat *fuse.Stat_t, isDir bool, attr p9.Attr) {
	*stat = fuse.Stat_t{}
	if isDir {
		stat.Mode = fuse.S_IFDIR | 0o555
	} else {
		stat.Mode = fuse.S_IFREG | 0o444
		if attr.Mode&0o200 != 0 {
			stat.Mode = fuse.S_IFREG | 0o644
		}
	}
	stat.Nlink = 1
	stat.Size = int64(attr.Size)
	now := fuse.Now()
	stat.Atim, stat.Mtim, stat.Ctim, stat.Birthtim = now, now, now, now
}

// Opendir opens a directory: walks the path, then lists its children via
// Server.ListChildren — the capability-filtered enumeration helper (see
// ninep.go) that applies the identical check Walk performs per entry — and
// caches the listing under a new file handle for Readdir.
func (a *winfsAdapter) Opendir(fusePath string) (int, uint64) {
	f, isDir, err := a.walk(fusePath)
	if err != nil {
		return -toFuseErrno(err), invalidFh
	}
	if !isDir {
		f.Close()
		return -fuse.ENOTDIR, invalidFh
	}
	entries := a.ns.ListChildren(nsPath(fusePath), a.cap)
	fh := a.allocFh()
	a.mu.Lock()
	a.handles[fh] = &openHandle{file: f, isDir: true, entries: entries}
	a.mu.Unlock()
	return 0, fh
}

// Readdir lists a directory's children. The listing was captured at Opendir
// via the namespace's own capability-filtered ListChildren; Readdir here only
// formats it for cgofuse — it performs no additional namespace access.
func (a *winfsAdapter) Readdir(fusePath string,
	fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64, fh uint64) int {
	fill(".", nil, 0)
	fill("..", nil, 0)
	a.mu.Lock()
	h, ok := a.handles[fh]
	a.mu.Unlock()
	if !ok {
		return -fuse.EBADF
	}
	for _, name := range h.entries {
		fill(name, nil, 0)
	}
	return 0
}

// Releasedir closes a directory handle opened by Opendir.
func (a *winfsAdapter) Releasedir(fusePath string, fh uint64) int {
	return a.release(fh)
}

// Open opens a file. Only the namespace's existing openable leaves (.../ctl,
// .../info, and /cer/fs/<path> files) succeed — exactly the set wire.go's
// node.Open permits; every other path is denied by the 9P server itself
// before this code ever sees a result, so there is nothing to relax here.
func (a *winfsAdapter) Open(fusePath string, flags int) (int, uint64) {
	f, isDir, err := a.walk(fusePath)
	if err != nil {
		return -toFuseErrno(err), invalidFh
	}
	if isDir {
		f.Close()
		return -fuse.EISDIR, invalidFh
	}
	mode := p9.ReadOnly
	switch flags & fuse.O_ACCMODE {
	case fuse.O_WRONLY:
		mode = p9.WriteOnly
	case fuse.O_RDWR:
		mode = p9.ReadWrite
	}
	if _, _, err := f.Open(mode); err != nil {
		f.Close()
		return -toFuseErrno(err), invalidFh
	}
	fh := a.allocFh()
	a.mu.Lock()
	a.handles[fh] = &openHandle{file: f}
	a.mu.Unlock()
	return 0, fh
}

// Read reads the descriptor bytes captured at Open (the ctl/info/fs-write
// DataEndpoint JSON, or the info JSON) — never device bytes, upholding the
// vertical 04 §3.5 invariant end to end through the mount.
func (a *winfsAdapter) Read(fusePath string, buff []byte, ofst int64, fh uint64) int {
	a.mu.Lock()
	h, ok := a.handles[fh]
	a.mu.Unlock()
	if !ok {
		return -fuse.EBADF
	}
	if h.data == nil {
		data, err := readAllFrom(h.file)
		if err != nil && len(data) == 0 {
			return -toFuseErrno(err)
		}
		h.data = data
	}
	if ofst < 0 || ofst >= int64(len(h.data)) {
		return 0
	}
	return copy(buff, h.data[ofst:])
}

func readAllFrom(f p9.File) ([]byte, error) {
	var out []byte
	buf := make([]byte, 4096)
	var off int64
	for {
		n, err := f.ReadAt(buf, off)
		out = append(out, buf[:n]...)
		off += int64(n)
		if err == io.EOF || n == 0 {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// Write writes to an opened /cer/fs file. This forwards to the 9P client's
// WriteAt, which server-side is only reachable on a /cer/fs write-open node
// (wire.go's node.Open dispatches such writes to Server.OpenFSWrite; a device
// ctl/info leaf is refused server-side already — see fs.go and wire.go). This
// method adds no separate authority check of its own because the one real
// authority (Server.OpenFSWrite/Server.Open) already ran during Open above.
func (a *winfsAdapter) Write(fusePath string, buff []byte, ofst int64, fh uint64) int {
	a.mu.Lock()
	h, ok := a.handles[fh]
	a.mu.Unlock()
	if !ok {
		return -fuse.EBADF
	}
	n, err := h.file.WriteAt(buff, ofst)
	if err != nil {
		return -toFuseErrno(err)
	}
	return n
}

// Release closes a file handle opened by Open.
func (a *winfsAdapter) Release(fusePath string, fh uint64) int {
	return a.release(fh)
}

func (a *winfsAdapter) release(fh uint64) int {
	a.mu.Lock()
	h, ok := a.handles[fh]
	delete(a.handles, fh)
	a.mu.Unlock()
	if !ok {
		return -fuse.EBADF
	}
	_ = h.file.Close()
	return 0
}

func (a *winfsAdapter) allocFh() uint64 {
	return atomic.AddUint64(&a.nextFh, 1)
}

// Statfs reports the same nominal, read-only summary wire.go's node.StatFS
// reports over the wire (the namespace is a control plane, not a data store).
func (a *winfsAdapter) Statfs(fusePath string, stat *fuse.Statfs_t) int {
	f, _, err := a.walk(fusePath)
	if err != nil {
		return -toFuseErrno(err)
	}
	defer f.Close()
	fsstat, err := f.StatFS()
	if err != nil {
		return -toFuseErrno(err)
	}
	stat.Bsize = uint64(fsstat.BlockSize)
	stat.Frsize = uint64(fsstat.BlockSize)
	stat.Namemax = uint64(fsstat.NameLength)
	return 0
}

var _ fuse.FileSystemInterface = (*winfsAdapter)(nil)
