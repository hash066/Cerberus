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

// AcquireLock is the single-instance guard. Winning is decided by an atomic
// O_CREATE|O_EXCL file create, not by a separate read-then-write: two
// processes racing to start at the same instant can't both observe "no live
// holder" and both proceed, because only one O_EXCL create can ever succeed
// for a given path. (An earlier version of this function did read-then-write,
// which had exactly that TOCTOU window -- fixed here.)
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
		f, err := os.OpenFile(LockPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := f.WriteString(fmt.Sprintf("%d", os.Getpid()))
			cerr := f.Close()
			if werr != nil {
				return fmt.Errorf("discovery: write lock: %w", werr)
			}
			if cerr != nil {
				return fmt.Errorf("discovery: close lock: %w", cerr)
			}
			return nil
		}
		if !os.IsExist(err) {
			return fmt.Errorf("discovery: create lock: %w", err)
		}

		b, rerr := os.ReadFile(LockPath())
		if rerr != nil {
			// Lost a race with a concurrent reclaim (the file vanished between
			// our failed create and this read) -- just retry from the top.
			continue
		}
		var pid int
		if _, scanErr := fmt.Sscanf(string(b), "%d", &pid); scanErr == nil && pid > 0 && IsRunning(pid) {
			return ErrAlreadyRunning
		}
		// Stale: reclaim and retry the exclusive create. If another process
		// reclaims first, our next O_EXCL attempt simply fails and loops again.
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
