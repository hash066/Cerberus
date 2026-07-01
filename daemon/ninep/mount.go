package ninep

import contract "github.com/hash066/cerberus/contract/go"

// MountConfig describes a requested FUSE (Unix) / WinFsp (Windows) mount of the
// 9P namespace into the host filesystem so Explorer/Finder/VFS can browse it.
type MountConfig struct {
	// Mountpoint is the host path to mount at (e.g. "/mnt/cerberus" or "C:\\cer").
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
// On Windows (see mount_windows.go) this is a REAL mount: it hosts a
// github.com/winfsp/cgofuse FileSystemInterface backed by a 9P2000.L client
// dialed in-process against cfg.NS via DialCap, so every Getattr/Open/Read/
// Readdir callback re-enters the SAME capability-checked Walk/Open path
// wire.go already enforces for network peers. If the WinFsp driver itself is
// not installed on the host, Mount fails fast with a specific, actionable
// error (see mount_windows.go) instead of a generic failure or a raw panic —
// preserving the project's maturity-honesty posture on hosts that genuinely
// lack the driver, while making the mount real on hosts that have it.
//
// On non-Windows platforms (see mount_unsupported.go) this remains a
// documented stub: a Unix mount needs hanwen/go-fuse (or cgofuse's Unix FUSE
// binding) wired the same way, which is out of scope for this change — the
// task is Windows-only for now (matching the sibling audio/lifecycle
// per-platform OS-stub convention: real OS integration lands one build tag at
// a time, never faked).
func Mount(cfg MountConfig) error {
	return mount(cfg)
}

// Unmount detaches a filesystem previously mounted with Mount.
//
// On Windows (mount_windows.go) this cleanly unmounts a live cgofuse host. On
// non-Windows platforms (mount_unsupported.go) it reports the same documented
// stub error as Mount, since nothing can have been mounted there.
func Unmount(cfg MountConfig) error {
	return unmount(cfg)
}
