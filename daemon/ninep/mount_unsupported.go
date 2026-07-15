//go:build !windows || cgo

// The cgo exclusion on Windows: cgofuse's cgo variant needs the WinFsp SDK's
// FUSE headers at compile time, which a cgo-enabled build (e.g. -tags ffi with
// a C toolchain) does not otherwise require. The shipped Windows mount uses
// cgofuse's nocgo DLL binding (mount_windows.go, `windows && !cgo`); a
// cgo-enabled Windows build gets this honest stub instead of a build error.

package ninep

import contract "github.com/hash066/cerberus/contract/go"

// mount is a DOCUMENTED STUB on non-Windows platforms, not a working mount.
//
// A real Unix mount needs a kernel filesystem driver — hanwen/go-fuse or
// cgofuse's Unix FUSE binding (libfuse/libfuse3) per docs/verticals/04 §6 —
// wired the same way mount_windows.go wires WinFsp: the FUSE callbacks would
// dial the namespace with DialCap and translate kernel VFS ops into 9P
// Twalk/Topen/Tread, so the same capability gating and ctl-returns-endpoint
// invariant would hold through the mount. That wiring is out of scope for
// this change (Windows-only per task scoping); faking it would violate the
// project's maturity-honesty rule (CLAUDE.md). The wire server in wire.go IS
// real and is exactly what a Unix FUSE bridge would attach to.
//
// Returns ErrPartitioned (transport/mount unavailable) until a driver-backed
// implementation lands for this platform.
func mount(_ MountConfig) error {
	return contract.Errf(contract.ErrPartitioned,
		"FUSE mount is a documented stub on this platform: requires a kernel fs driver "+
			"(hanwen/go-fuse or cgofuse's Unix FUSE binding) not wired in this build; "+
			"the 9P2000.L wire server (wire.go) is the real transport it would bridge to. "+
			"See mount_windows.go for the real WinFsp+cgofuse implementation on Windows.")
}

// unmount is the corresponding documented stub: nothing can have been mounted
// on this platform, so there is nothing to detach.
func unmount(_ MountConfig) error {
	return contract.Errf(contract.ErrPartitioned,
		"FUSE mount is a documented stub on this platform: nothing was mounted to unmount")
}
