package discovery

import (
	"os"
	"sync"
	"testing"
	"time"
)

// isolateConfigDir points auth.OperatorTokenPath() -- and therefore the
// discovery manifest/lock directory (dir(), which is filepath.Dir of it) -- at
// a fresh per-test temp directory, on EVERY OS. os.UserConfigDir() reads a
// different env var per platform: %AppData% on Windows, $XDG_CONFIG_HOME on
// Linux, and $HOME/Library/Application Support on macOS. Overriding only one of
// them isolates the tests on that OS but silently leaves them sharing the real
// user config dir on the others -- which is exactly how the lock-file tests
// below leaked state into each other on the Linux/macOS CI runners (one test's
// AcquireLock left a live lock that the next test then saw), while passing on
// the Windows dev box. Setting all three points every platform at t.TempDir(),
// which the test framework also cleans up automatically.
func isolateConfigDir(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("AppData", tmp)         // Windows
	t.Setenv("XDG_CONFIG_HOME", tmp) // Linux (and other XDG platforms)
	t.Setenv("HOME", tmp)            // macOS (and Linux fallback when XDG unset)
}

func TestWriteReadRoundTrip(t *testing.T) {
	isolateConfigDir(t)

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
	isolateConfigDir(t)
	if _, err := Read(); err == nil {
		t.Fatal("expected an error reading a manifest that was never written")
	}
}

func TestAcquireLockRejectsSecondLiveHolder(t *testing.T) {
	isolateConfigDir(t)
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

// TestAcquireLockIsRaceFree proves the TOCTOU window a prior read-then-write
// implementation had is actually closed: many goroutines race to call
// AcquireLock at the same instant (all sharing this process's PID, since
// that's the only way to exercise the race within a single test binary), and
// exactly one must win. A read-then-write implementation would let more than
// one goroutine observe "no live holder" in the same window and both succeed;
// the O_CREATE|O_EXCL implementation cannot, because only one exclusive
// create for a given path can ever succeed at the OS level.
func TestAcquireLockIsRaceFree(t *testing.T) {
	isolateConfigDir(t)
	const n = 64
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- AcquireLock()
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	successes, alreadyRunning := 0, 0
	for err := range results {
		switch err {
		case nil:
			successes++
		case ErrAlreadyRunning:
			alreadyRunning++
		default:
			t.Fatalf("unexpected error from a racing AcquireLock: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 winner among %d racing callers, got %d (successes=%d, rejected=%d)", n, successes, successes, alreadyRunning)
	}
	if alreadyRunning != n-1 {
		t.Fatalf("expected the other %d callers to be rejected, got %d", n-1, alreadyRunning)
	}
}

func TestAcquireLockReclaimsStaleLock(t *testing.T) {
	isolateConfigDir(t)
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
	isolateConfigDir(t)
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
