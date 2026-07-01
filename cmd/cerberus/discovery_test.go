package main

// discovery_test.go covers the CLI's address-resolution priority order:
// explicit flag/env > live discovery manifest > hardcoded default. Only the
// manifest layer (resolveAddr / liveManifestAddr) is exercised here in
// isolation — the flag/env layer is a couple of lines in run() covered by
// existing extractValueFlag tests, and this file is not the place to spawn a
// real cerberusd (see test/e2e for that pattern).

import (
	"os"
	"testing"
	"time"

	"github.com/hash066/cerberus/daemon/discovery"
)

// isolateManifestDir points the discovery package at a fresh temp dir for the
// duration of the test, mirroring daemon/discovery's own test helper.
func isolateManifestDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
}

func TestResolveAddr_EnvOverridesManifest(t *testing.T) {
	isolateManifestDir(t)
	if err := discovery.Write(discovery.Manifest{
		PID:     os.Getpid(), // this test process is alive
		RPCAddr: "127.0.0.1:19092",
	}); err != nil {
		t.Fatalf("Write manifest: %v", err)
	}
	t.Setenv("CERBERUS_TEST_ADDR", "127.0.0.1:1")

	got := resolveAddr("CERBERUS_TEST_ADDR", "127.0.0.1:9092", func(m discovery.Manifest) string { return m.RPCAddr })
	if got != "127.0.0.1:1" {
		t.Fatalf("resolveAddr = %q, want env value to win over a live manifest", got)
	}
}

func TestResolveAddr_LiveManifestOverridesDefault(t *testing.T) {
	isolateManifestDir(t)
	if err := discovery.Write(discovery.Manifest{
		PID:     os.Getpid(), // alive: the test process itself
		RPCAddr: "127.0.0.1:19092",
	}); err != nil {
		t.Fatalf("Write manifest: %v", err)
	}
	t.Setenv("CERBERUS_TEST_ADDR_UNSET", "") // ensure absent

	got := resolveAddr("CERBERUS_TEST_ADDR_UNSET", "127.0.0.1:9092", func(m discovery.Manifest) string { return m.RPCAddr })
	if got != "127.0.0.1:19092" {
		t.Fatalf("resolveAddr = %q, want the live manifest's address", got)
	}
}

func TestResolveAddr_StaleManifestFallsBackToDefault(t *testing.T) {
	isolateManifestDir(t)
	// A PID vanishingly unlikely to be alive on this host (mirrors
	// daemon/discovery's own stale-lock test convention).
	const deadPID = 999999
	if discovery.IsRunning(deadPID) {
		t.Skip("PID 999999 happens to be live on this host; cannot exercise the stale-manifest path")
	}
	if err := discovery.Write(discovery.Manifest{
		PID:       deadPID,
		StartedAt: time.Now().Add(-time.Hour),
		RPCAddr:   "127.0.0.1:19092",
	}); err != nil {
		t.Fatalf("Write manifest: %v", err)
	}

	got := resolveAddr("CERBERUS_TEST_ADDR_UNSET2", "127.0.0.1:9092", func(m discovery.Manifest) string { return m.RPCAddr })
	if got != "127.0.0.1:9092" {
		t.Fatalf("resolveAddr = %q, want the hardcoded default when the manifest's PID is dead", got)
	}
}

func TestResolveAddr_MissingManifestFallsBackToDefault(t *testing.T) {
	isolateManifestDir(t)
	// Nothing written -- discovery.Read() must fail with the dir isolated.

	got := resolveAddr("CERBERUS_TEST_ADDR_UNSET3", "127.0.0.1:9092", func(m discovery.Manifest) string { return m.RPCAddr })
	if got != "127.0.0.1:9092" {
		t.Fatalf("resolveAddr = %q, want the hardcoded default when there is no manifest at all", got)
	}
}

func TestResolveAddr_LiveManifestWithEmptyFieldFallsBackToDefault(t *testing.T) {
	isolateManifestDir(t)
	// The manifest exists and its PID is alive, but the specific subsystem
	// (e.g. metrics) never bound at all -- its field is empty. That should
	// still fall through to the hardcoded default, not an empty string.
	if err := discovery.Write(discovery.Manifest{
		PID:     os.Getpid(),
		RPCAddr: "127.0.0.1:19092",
		// MetricsAddr intentionally left empty.
	}); err != nil {
		t.Fatalf("Write manifest: %v", err)
	}

	got := resolveAddr("CERBERUS_TEST_ADDR_UNSET4", "127.0.0.1:7779", func(m discovery.Manifest) string { return m.MetricsAddr })
	if got != "127.0.0.1:7779" {
		t.Fatalf("resolveAddr = %q, want the hardcoded default when the manifest's field is empty", got)
	}
}
