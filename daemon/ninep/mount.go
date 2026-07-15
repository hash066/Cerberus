package ninep

import (
	"io"
	"path"
	"strings"
	"sync"

	"github.com/hugelgupf/p9/p9"

	contract "github.com/hash066/cerberus/contract/go"
)

// MountConfig describes a requested FUSE (Linux) / WinFsp (Windows) mount of the
// 9P namespace into the host filesystem so Explorer/a file manager/VFS can browse it.
type MountConfig struct {
	// Mountpoint is the host path to mount at (e.g. "/mnt/cerberus" or "X:").
	Mountpoint string
	// Cap is the capability the mounted view is scoped to (per-principal namespace).
	Cap contract.CapHandle
	// NS is the capability-gated namespace to present. Every filesystem callback
	// (Getattr/Open/Read/Readdir/...) is served by walking/opening THIS namespace
	// over the real 9P2000.L wire (see wire.go's DialCap), so the mount is a
	// second TRANSPORT onto the same capability-checked Server, never a second,
	// unguarded access path (CLAUDE.md golden rule 5: no ambient authority).
	NS *Server
}

// Mount mounts the 9P capability namespace as a real host filesystem.
//
// Mount does NOT block. On every platform the kernel driver's dispatch loop is
// handed to a background goroutine and Mount returns: nil once the filesystem
// is live, or a specific *contract.CapError describing exactly why it is not.
// Call Unmount to tear it down.
//
// Which builds get a real mount (the tag matrix lives in mount_unsupported.go):
//
//   - Windows WITHOUT cgo (mount_windows.go): REAL, via github.com/winfsp/cgofuse's
//     nocgo binding against the WinFsp driver. This is the repo's default build.
//   - Windows WITH cgo: the documented stub — not because the mount is missing,
//     but because cgofuse's cgo variant would need the WinFsp SDK's headers at
//     compile time. Build without cgo to get the real thing.
//   - Linux (mount_linux.go), cgo or not: REAL, via github.com/hanwen/go-fuse,
//     which talks the FUSE protocol to /dev/fuse directly in PURE GO — no cgo, no
//     libfuse. Nothing to install: fuse is in the mainline kernel.
//   - macOS/BSD: the documented stub (see mountUnsupportedMsg).
//
// Every real mount dials the namespace with DialCap, so each callback re-enters
// the SAME capability-checked Walk/Open path wire.go enforces for network peers.
//
// When a platform's kernel driver is simply not installed on this host, Mount
// fails fast with an actionable error naming it (CLAUDE.md "Maturity honesty")
// rather than a generic failure, a raw panic, or a silently-faked success.
func Mount(cfg MountConfig) error {
	return mount(cfg)
}

// Unmount detaches a filesystem previously mounted with Mount.
func Unmount(cfg MountConfig) error {
	return unmount(cfg)
}

// --- shared mount bookkeeping ----------------------------------------------

// mountHandle is one live mount, however the platform implements it (a cgofuse
// FileSystemHost on Windows, a go-fuse Server on Linux). The registry below is
// shared so both platforms get identical duplicate-mount and unknown-mountpoint
// semantics from one implementation.
type mountHandle interface {
	// unmount detaches the filesystem and releases its 9P client.
	unmount() error
}

var (
	mountsMu sync.Mutex
	mounts   = map[string]mountHandle{}
)

// registerMount records a live mount, refusing a second mount of the same
// mountpoint by this process.
func registerMount(mountpoint string, h mountHandle) error {
	mountsMu.Lock()
	defer mountsMu.Unlock()
	if _, exists := mounts[mountpoint]; exists {
		return contract.Errf(contract.ErrDenied, "mount: "+mountpoint+" is already mounted by this process")
	}
	mounts[mountpoint] = h
	return nil
}

// unregisterMount forgets a mountpoint (used to roll back a failed mount).
func unregisterMount(mountpoint string) {
	mountsMu.Lock()
	defer mountsMu.Unlock()
	delete(mounts, mountpoint)
}

// takeMount removes and returns the mount registered at mountpoint.
func takeMount(mountpoint string) (mountHandle, bool) {
	mountsMu.Lock()
	defer mountsMu.Unlock()
	h, ok := mounts[mountpoint]
	if ok {
		delete(mounts, mountpoint)
	}
	return h, ok
}

// unmountRegistered is the shared body of every platform's unmount: find the
// mount, tear it down, and report an unknown mountpoint consistently.
func unmountRegistered(mountpoint string) error {
	h, ok := takeMount(mountpoint)
	if !ok {
		return contract.Errf(contract.ErrDenied, "unmount: "+mountpoint+" is not mounted by this process")
	}
	return h.unmount()
}

// --- shared path helpers ----------------------------------------------------

// components splits a mount-relative path ("/", "/dev", "/dev/vram/AA/0/ctl")
// into the name components p9.File.Walk expects, relative to the attached /cer
// root.
func components(p string) []string {
	clean := path.Clean("/" + p)
	if clean == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(clean, "/"), "/")
}

// nsPath rebuilds the full namespace path (e.g. "/cer/dev/vram/AA/0/ctl") a
// mount-relative path corresponds to, for calling ListChildren (which, like the
// rest of the Server API, takes namespace-rooted paths).
func nsPath(p string) string {
	names := components(p)
	if len(names) == 0 {
		return rootPath
	}
	return rootPath + "/" + strings.Join(names, "/")
}

// readAllFrom drains an opened 9P file's descriptor bytes (the ctl/info/fs-write
// DataEndpoint JSON). It is never device bytes: the 9P server only ever exposes
// the descriptor captured at Open (see wire.go's node.ReadAt), upholding the
// vertical 04 §3.5 invariant through every mount.
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
