package discovery

import (
	"os"
	"testing"
	"time"
)

func TestWriteReadRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// os.UserConfigDir() on non-Windows honors XDG_CONFIG_HOME; on Windows it
	// uses %AppData%, so also override that for this test to be isolated.
	t.Setenv("AppData", t.TempDir())

	m := Manifest{
		Version:     "0.1.0",
		PID:         os.Getpid(),
		StartedAt:   time.Now().UTC(),
		Profile:     "open_mesh",
		Site:        "site-a",
		GatewayAddr: "127.0.0.1:8080",
		APIAddr:     "127.0.0.1:7777",
		MetricsAddr: "127.0.0.1:7779",
		RPCAddr:     "127.0.0.1:9092",
		TokenPath:   "/tmp/operator.token",
	}
	if err := Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.RPCAddr != m.RPCAddr || got.GatewayAddr != m.GatewayAddr || got.Site != m.Site {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, m)
	}
}

func TestReadMissingManifestErrors(t *testing.T) {
	t.Setenv("AppData", t.TempDir())
	if _, err := Read(); err == nil {
		t.Fatal("expected an error reading a manifest that was never written")
	}
}

func TestAcquireLockRejectsSecondLiveHolder(t *testing.T) {
	t.Setenv("AppData", t.TempDir())
	if err := AcquireLock(); err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	// The lock file now names THIS test process's own PID, which is alive, so
	// a second acquire attempt (simulating a second daemon instance) must be
	// rejected rather than silently racing the first for the same ports.
	if err := AcquireLock(); err != ErrAlreadyRunning {
		t.Fatalf("second AcquireLock: got %v, want ErrAlreadyRunning", err)
	}
}

func TestAcquireLockReclaimsStaleLock(t *testing.T) {
	t.Setenv("AppData", t.TempDir())
	// A PID that is vanishingly unlikely to be a live process on this host.
	const deadPID = 999999
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LockPath(), []byte("999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsRunning(deadPID) {
		t.Skip("PID 999999 happens to be live on this host; cannot exercise the stale-lock path")
	}
	if err := AcquireLock(); err != nil {
		t.Fatalf("AcquireLock should reclaim a stale lock, got: %v", err)
	}
}

func TestIsRunningTrueForSelf(t *testing.T) {
	if !IsRunning(os.Getpid()) {
		t.Fatal("the calling process must report as running")
	}
}

func TestRemoveDeletesBothFiles(t *testing.T) {
	t.Setenv("AppData", t.TempDir())
	if err := Write(Manifest{PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err := AcquireLock(); err != nil {
		t.Fatal(err)
	}
	Remove()
	if _, err := os.Stat(Path()); !os.IsNotExist(err) {
		t.Fatal("manifest file should be removed")
	}
	if _, err := os.Stat(LockPath()); !os.IsNotExist(err) {
		t.Fatal("lock file should be removed")
	}
}
