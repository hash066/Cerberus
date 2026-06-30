package ninep

import contract "github.com/hash066/cerberus/contract/go"

// MountConfig describes a requested FUSE (Unix) / WinFsp (Windows) mount of the
// 9P namespace into the host filesystem so Explorer/Finder/VFS can browse it.
type MountConfig struct {
	// Mountpoint is the host path to mount at (e.g. "/mnt/cerberus" or "C:\\cer").
	Mountpoint string
	// Cap is the capability the mounted view is scoped to (per-principal namespace).
	Cap contract.CapHandle
}

// Mount is a DOCUMENTED STUB, not a working mount.
//
// A real mount needs a kernel filesystem driver — hanwen/go-fuse (Linux/macOS)
// or WinFsp + cgofuse (Windows) per docs/verticals/04 §6 — which requires the
// driver installed and (usually) elevated privileges, and cannot be exercised
// in this headless build/CI environment. Faking it would violate the project's
// maturity-honesty rule (CLAUDE.md). The wire server in wire.go IS real and the
// FUSE/WinFsp bridge would attach to it: the mount layer translates kernel VFS
// ops into 9P Twalk/Topen/Tread against a DialCap connection, so the same
// capability gating and ctl-returns-endpoint invariant hold through the mount.
//
// Returns ErrPartitioned (transport/mount unavailable) until a driver-backed
// implementation lands.
func Mount(_ MountConfig) error {
	return contract.Errf(contract.ErrPartitioned,
		"FUSE/WinFsp mount is a documented stub: requires a kernel fs driver "+
			"(go-fuse / WinFsp+cgofuse) not available in this environment; "+
			"the 9P2000.L wire server (wire.go) is the real transport it would bridge to")
}
