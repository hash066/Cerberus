//go:build !linux && (!windows || cgo)

// This file is the leftover: every (GOOS, cgo) combination that has no real
// mount. The build-tag matrix across the three mount files is:
//
//	GOOS=windows, !cgo → mount_windows.go   REAL (cgofuse's nocgo WinFsp DLL binding)
//	GOOS=windows,  cgo → THIS FILE          stub (see the cgo exclusion below)
//	GOOS=linux,   !cgo → mount_linux.go     REAL (pure-Go hanwen/go-fuse)
//	GOOS=linux,    cgo → mount_linux.go     REAL — go-fuse is pure Go, so cgo is irrelevant to it
//	everything else    → THIS FILE          stub (macOS/BSD; see mountUnsupportedMsg)
//
// THE cgo EXCLUSION ON WINDOWS: cgofuse's cgo variant needs the WinFsp SDK's
// FUSE headers at compile time (its host_cgo.go carries
// `#cgo windows CFLAGS: -I/usr/local/include/winfsp`), which a cgo-enabled
// build (e.g. -tags ffi with a C toolchain) does not otherwise require. The
// shipped Windows mount uses cgofuse's nocgo DLL binding (mount_windows.go,
// `windows && !cgo`); a cgo-enabled Windows build gets this honest stub instead
// of a build error.
//
// Note that Linux needs NO cgo term at all: hanwen/go-fuse speaks the FUSE
// protocol to /dev/fuse in pure Go (its `fs` and `fuse` packages both report an
// empty CgoFiles list), so the Linux mount is real whether or not cgo is
// enabled. The cgo/mount conflict is a Windows-and-cgofuse problem specifically,
// not a property of FUSE mounts in general.

package ninep

import (
	"runtime"

	contract "github.com/hash066/cerberus/contract/go"
)

// mount is a DOCUMENTED STUB for every build this file covers (see the tag
// matrix above): macOS/BSD on any build, plus a cgo-enabled Windows build. It is
// not a working mount.
//
// Windows without cgo (mount_windows.go, cgofuse+WinFsp) and Linux
// (mount_linux.go, pure-Go hanwen/go-fuse against /dev/fuse) are both REAL. This
// file is what is left over, and the honest reason differs per case:
//
//   - A cgo-enabled WINDOWS build has a real mount available in principle — the
//     driver and the code both exist — but reaching it would require the WinFsp
//     SDK's FUSE headers at compile time (see the cgo exclusion above). Rather
//     than fail the build of a daemon whose mount is optional, it reports this.
//     Build without cgo to get the real Windows mount. This case is a
//     TOOLCHAIN limit, not a missing implementation.
//   - macOS has NO pure-Go option. The kernel has no built-in FUSE; a mount
//     needs macFUSE, a third-party kernel extension the user must install,
//     approve in System Settings under a security prompt, and reboot for —
//     and which is closed-source since 4.x with licence terms that restrict
//     commercial redistribution. Binding it also needs cgo, which would break
//     this repo's CGO_ENABLED=0 default build (CLAUDE.md). Apple's supported
//     replacement, FSKit, is available only on macOS 15+ and requires a
//     provisioned app extension rather than a daemon flag. None of that is
//     work this file can hide: it is a product decision about what to ask a
//     macOS user to install. Until that decision is made, this reports the
//     truth rather than shipping a mount a user cannot actually use.
//   - The BSDs have kernel FUSE, but no one has verified a mount here and
//     go-fuse's support is Linux-specific in places, so claiming it would be
//     unverified.
//
// The 9P2000.L wire server (wire.go) IS real on every platform and is exactly
// what a mount bridges to — so on these platforms the namespace remains fully
// usable over 9P, and only the convenience of a mountpoint is missing.
//
// Returns ErrPartitioned (transport/mount unavailable), naming the platform and
// precisely what is missing (CLAUDE.md "Maturity honesty").
func mount(_ MountConfig) error {
	return contract.Errf(contract.ErrPartitioned, mountUnsupportedMsg())
}

// unmount is the corresponding documented stub: nothing can have been mounted
// on this platform, so there is nothing to detach.
func unmount(_ MountConfig) error {
	return contract.Errf(contract.ErrPartitioned,
		"9P mount is not implemented on "+runtime.GOOS+": nothing was mounted to unmount")
}

// mountUnsupportedMsg names exactly what is missing on this platform, so the
// error is actionable rather than a generic "unsupported".
func mountUnsupportedMsg() string {
	if runtime.GOOS == "windows" {
		// Only reachable in a cgo-enabled Windows build (the tag matrix above):
		// the real mount exists, this build just cannot compile it.
		return "9P mount is unavailable in this build: it is a CGO-ENABLED WINDOWS build, and the real " +
			"WinFsp mount (mount_windows.go) is compiled only into non-cgo Windows builds. cgofuse's cgo " +
			"variant would need the WinFsp SDK's FUSE headers at compile time " +
			"(#cgo windows CFLAGS: -I/usr/local/include/winfsp), which this build does not provide. " +
			"The mount itself is real and works — rebuild with CGO_ENABLED=0 (the repo default) to get it. " +
			"Nothing is missing at runtime beyond this build choice; the 9P2000.L wire server (wire.go) " +
			"is real in this build and the namespace is fully usable over it."
	}
	if runtime.GOOS == "darwin" {
		return "9P mount is not implemented on macOS: the macOS kernel has no built-in FUSE, so a mount " +
			"requires either macFUSE (a third-party kernel extension the user must separately install, " +
			"approve in System Settings, and reboot for — and whose licence restricts commercial " +
			"redistribution) or FSKit (macOS 15+, and only from a provisioned app extension, not a daemon " +
			"flag). Either route also needs cgo, which this build does not use. This is a deliberate, " +
			"documented gap, not an oversight — the 9P2000.L wire server (wire.go) is real here and the " +
			"namespace is fully usable over it; only the mountpoint convenience is absent. " +
			"Windows (WinFsp) and Linux (pure-Go go-fuse) both have real mounts."
	}
	return "9P mount is not implemented on " + runtime.GOOS + ": no verified FUSE binding is wired for this " +
		"platform in this build. The 9P2000.L wire server (wire.go) is real here and the namespace is fully " +
		"usable over it. Windows (WinFsp via cgofuse) and Linux (pure-Go hanwen/go-fuse) have real mounts; " +
		"see mount_windows.go / mount_linux.go for the shape a port would follow."
}
