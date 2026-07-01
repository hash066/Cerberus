package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hash066/cerberus/daemon/discovery"
)

// isolateConfigDir points the OS user-config-dir env vars at a fresh temp dir,
// so discovery.Path()/auth.OperatorTokenPath() (which discovery derives its
// directory from) never touch the real machine's Cerberus config during tests.
func isolateConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	// os.UserConfigDir() reads a different var per OS. Overriding only XDG/AppData
	// leaves macOS ($HOME/Library/Application Support) pointed at the real user
	// dir, where a concurrently-running package's test (e.g. cmd/cerberus's
	// discovery_test) may have left a daemon.json manifest — which resolveAddrs()
	// would then read instead of the intended default. Set HOME too so this test
	// is isolated on every OS.
	t.Setenv("XDG_CONFIG_HOME", dir) // Linux
	t.Setenv("AppData", dir)         // Windows
	t.Setenv("HOME", dir)            // macOS (and Linux fallback when XDG unset)
}

func TestResolveAddrs_HardcodedDefaultsWhenNothingElsePresent(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv("CERBERUS_RPC_ADDR", "")
	t.Setenv("CERBERUS_METRICS_ADDR", "")

	got := resolveAddrs()
	if got.RPCAddr != defaultRPCAddr {
		t.Errorf("RPCAddr = %q, want default %q", got.RPCAddr, defaultRPCAddr)
	}
	if got.MetricsAddr != defaultMetricsAddr {
		t.Errorf("MetricsAddr = %q, want default %q", got.MetricsAddr, defaultMetricsAddr)
	}
}

func TestResolveAddrs_EnvOverrideWins(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv("CERBERUS_RPC_ADDR", "127.0.0.1:19092")
	t.Setenv("CERBERUS_METRICS_ADDR", "127.0.0.1:17779")

	got := resolveAddrs()
	if got.RPCAddr != "127.0.0.1:19092" {
		t.Errorf("RPCAddr = %q, want env override", got.RPCAddr)
	}
	if got.MetricsAddr != "127.0.0.1:17779" {
		t.Errorf("MetricsAddr = %q, want env override", got.MetricsAddr)
	}
}

func TestResolveAddrs_DiscoveryManifestUsedWhenPresent(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv("CERBERUS_RPC_ADDR", "")
	t.Setenv("CERBERUS_METRICS_ADDR", "")

	if err := discovery.Write(discovery.Manifest{
		RPCAddr:     "127.0.0.1:29092",
		MetricsAddr: "127.0.0.1:27779",
	}); err != nil {
		t.Fatalf("discovery.Write: %v", err)
	}

	got := resolveAddrs()
	if got.RPCAddr != "127.0.0.1:29092" {
		t.Errorf("RPCAddr = %q, want manifest value", got.RPCAddr)
	}
	if got.MetricsAddr != "127.0.0.1:27779" {
		t.Errorf("MetricsAddr = %q, want manifest value", got.MetricsAddr)
	}
}

func TestResolveAddrs_EnvOverridesDiscoveryManifest(t *testing.T) {
	isolateConfigDir(t)
	if err := discovery.Write(discovery.Manifest{
		RPCAddr:     "127.0.0.1:29092",
		MetricsAddr: "127.0.0.1:27779",
	}); err != nil {
		t.Fatalf("discovery.Write: %v", err)
	}
	t.Setenv("CERBERUS_RPC_ADDR", "127.0.0.1:39092")

	got := resolveAddrs()
	if got.RPCAddr != "127.0.0.1:39092" {
		t.Errorf("RPCAddr = %q, want env override even with a manifest present", got.RPCAddr)
	}
}

func TestResolveAddrs_PartialManifestFallsBackToDefaultsPerField(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv("CERBERUS_RPC_ADDR", "")
	t.Setenv("CERBERUS_METRICS_ADDR", "")

	// A manifest that only names the RPC addr (e.g. metrics subsystem failed
	// to bind on that instance) should still yield the hardcoded default for
	// the field it left empty, not an empty string a client would fail to
	// dial.
	if err := discovery.Write(discovery.Manifest{RPCAddr: "127.0.0.1:29092"}); err != nil {
		t.Fatalf("discovery.Write: %v", err)
	}

	got := resolveAddrs()
	if got.RPCAddr != "127.0.0.1:29092" {
		t.Errorf("RPCAddr = %q, want manifest value", got.RPCAddr)
	}
	if got.MetricsAddr != defaultMetricsAddr {
		t.Errorf("MetricsAddr = %q, want default fallback for the empty manifest field", got.MetricsAddr)
	}
}

func TestResolveAddrs_ManifestPathIsUnderConfigDir(t *testing.T) {
	isolateConfigDir(t)
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}
	want := filepath.Join(dir, "cerberus", "daemon.json")
	if discovery.Path() != want {
		t.Fatalf("discovery.Path() = %q, want %q", discovery.Path(), want)
	}
}
