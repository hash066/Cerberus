// Package discovery is the daemon's self-description seam. It closes the
// biggest hole found in the v3 audit: cerberusd and its clients (cerberus CLI,
// the tray dashboard, an MCP server, any third-party tool) each hardcoded the
// SAME default ports independently and hoped they'd agree. On any conflict
// (another process already holds a port) that hope silently breaks — the
// daemon logs a line and keeps running with a subsystem quietly unreachable.
//
// This package gives every daemon instance a single, well-known, machine
// readable file describing what it ACTUALLY bound to (not what it hoped to),
// plus a PID-based single-instance guard. Any tool that wants to find "the
// Cerberus daemon on this machine" reads Path(), not a hardcoded port.
package discovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hash066/cerberus/daemon/auth"
)

// Manifest is what a running cerberusd writes to Path() once its subsystems
// have finished binding. Every *Addr field is the address the subsystem is
// ACTUALLY reachable on -- empty if that subsystem failed to bind (see
// daemon/system's server-supervisor loop, which now falls back to an
// OS-assigned ephemeral port rather than silently going dark on a conflict).
type Manifest struct {
	// Version is the contract.ContractVersion the daemon was built against, so
	// a client can detect a version skew before making an RPC.
	Version string `json:"version"`
	// PID is the daemon process id (also duplicated in the lock file so a
	// stale manifest and a stale lock agree on what to check).
	PID int `json:"pid"`
	// StartedAt is when this instance came up (RFC3339). Lets a client tell a
	// fresh manifest from a leftover one from a prior boot.
	StartedAt time.Time `json:"started_at"`
	// Profile is "open_mesh" or "sealed" (cmd/cerberusd's -profile flag).
	Profile string `json:"profile"`
	// Site is the mesh site name this instance joined.
	Site string `json:"site,omitempty"`

	GatewayAddr string `json:"gateway_addr,omitempty"`
	APIAddr     string `json:"api_addr,omitempty"`
	MetricsAddr string `json:"metrics_addr,omitempty"`
	RPCAddr     string `json:"rpc_addr,omitempty"`

	// TokenPath is auth.OperatorTokenPath() -- included so a client needs to
	// know only ONE path (this manifest's) to bootstrap everything else.
	TokenPath string `json:"token_path"`
}

// dir is the directory the manifest/lock files live in -- the same
// OS-standard config dir the operator token already uses, so there is exactly
// one "where does Cerberus keep its state" answer for the whole product.
func dir() string {
	return filepath.Dir(auth.OperatorTokenPath())
}

// Path is the well-known manifest location, e.g. on Windows
// %APPDATA%\cerberus\daemon.json. Any tool -- the CLI, the tray, an MCP
// server, a third-party integration -- reads this instead of a hardcoded port.
func Path() string {
	return filepath.Join(dir(), "daemon.json")
}

// LockPath is the single-instance guard file: it holds the PID of the daemon
// that currently owns it.
func LockPath() string {
	return filepath.Join(dir(), "daemon.lock")
}

// Write persists m to Path() as indented JSON (0600 -- it is not secret, but
// there is no reason to make it world-readable either). Called once cerberusd
// has finished binding its subsystems, so readers never observe a partial
// manifest from a half-started daemon.
func Write(m Manifest) error {
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		return fmt.Errorf("discovery: mkdir: %w", err)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("discovery: marshal: %w", err)
	}
	tmp := Path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("discovery: write: %w", err)
	}
	// Atomic on the same filesystem: a reader never sees a truncated file.
	return os.Rename(tmp, Path())
}

// Read loads the manifest a running daemon last wrote. It does NOT verify the
// daemon is still alive -- callers that care should cross-check
// IsRunning(Read().PID) or simply attempt to connect and fall back to
// defaults on failure, which every client in this repo already does.
func Read() (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(Path())
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("discovery: malformed manifest: %w", err)
	}
	return m, nil
}

// Remove deletes the manifest and lock files. Called on clean daemon
// shutdown so a stale manifest never outlives its process; a crash simply
// leaves them behind, which IsRunning/AcquireLock below are built to detect.
func Remove() {
	_ = os.Remove(Path())
	_ = os.Remove(LockPath())
}

// ErrAlreadyRunning is returned by AcquireLock when another live process
// already holds the lock file.
var ErrAlreadyRunning = errors.New("discovery: another cerberusd instance is already running")

// AcquireLock is the single-instance guard. Winning is decided by atomically
// hard-linking a temp file (already containing our PID) onto LockPath: like
// O_CREATE|O_EXCL, only one linker can win for a given path, so two processes
// racing to start can't both proceed -- but unlike O_EXCL it never exposes a
// created-but-not-yet-written empty file that a concurrent caller could misread
// as stale and unlink out from under the winner (that window let two callers
// both win on POSIX; see the loop body). An even earlier version did
// read-then-write, which had a plain TOCTOU window -- both are fixed here.
//
// If the lock file already exists, its PID is checked: a live PID rejects
// with ErrAlreadyRunning (even the caller's own -- cerberusd calls this
// exactly once at startup, so there is no legitimate "reacquire my own lock"
// case). A stale lock (unreadable PID, or the process no longer exists -- the
// prior daemon crashed) is removed and the create is retried; if that retry
// races with another reclaimer, the loop simply tries again.
func AcquireLock() error {
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		return fmt.Errorf("discovery: mkdir: %w", err)
	}
	for {
		// Publish the lock atomically WITH its content. A plain
		// O_CREATE|O_EXCL create makes an EMPTY file first and writes the PID as
		// a separate step; a racer that observes that momentarily-empty file
		// reads an unparseable PID, judges the lock stale, and unlinks it -- and
		// on POSIX unlinking a file another process still holds open SUCCEEDS --
		// so a second caller then wins its own create too, yielding TWO owners.
		// (Windows happens to mask this because it refuses to unlink an open
		// file; Linux/macOS do not -- which is why the race-free test caught 2
		// winners only off Windows.) Writing the PID into a unique temp file and
		// hard-linking it into place means LockPath, the instant it exists,
		// already carries a valid PID -- there is no empty window to misjudge.
		tmp, err := os.CreateTemp(dir(), "daemon.lock.*.tmp")
		if err != nil {
			return fmt.Errorf("discovery: create temp lock: %w", err)
		}
		tmpName := tmp.Name()
		_, werr := fmt.Fprintf(tmp, "%d", os.Getpid())
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			_ = os.Remove(tmpName)
			if werr != nil {
				return fmt.Errorf("discovery: write lock: %w", werr)
			}
			return fmt.Errorf("discovery: close lock: %w", cerr)
		}

		// os.Link fails with an IsExist error if LockPath already exists, giving
		// the same "exactly one creator wins" guarantee O_EXCL did, but for a
		// file that is already fully populated.
		linkErr := os.Link(tmpName, LockPath())
		_ = os.Remove(tmpName) // drop the temp name either way (the inode lives on via LockPath if we won)
		if linkErr == nil {
			return nil
		}
		if !os.IsExist(linkErr) {
			return fmt.Errorf("discovery: create lock: %w", linkErr)
		}

		b, rerr := os.ReadFile(LockPath())
		if rerr != nil {
			// Lost a race with a concurrent reclaim (the file vanished between
			// our failed link and this read) -- just retry from the top.
			continue
		}
		var pid int
		if _, scanErr := fmt.Sscanf(string(b), "%d", &pid); scanErr == nil && pid > 0 && IsRunning(pid) {
			return ErrAlreadyRunning
		}
		// Stale: reclaim and retry the exclusive create. If another process
		// reclaims first, our next Link attempt simply fails and loops again.
		_ = os.Remove(LockPath())
	}
}

// IsRunning reports whether a process with the given PID currently exists.
// Implemented per-OS (process_windows.go / process_unix.go) since the
// standard library's os.FindProcess always succeeds on POSIX regardless of
// whether the PID is live.
func IsRunning(pid int) bool {
	return isRunning(pid)
}
