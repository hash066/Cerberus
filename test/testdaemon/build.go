// Package testdaemon builds and locates cerberusd for integration tests (chaos
// real-process suite, e2e demo). A single shared binary under test/.testbin/
// avoids Windows TempDir cleanup flakes (cerberusd.exe locked inside t.TempDir)
// and serializes go build across concurrent test processes via an O_EXCL lock
// file so chaos + e2e never race the linker output or module cache.
package testdaemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

var (
	buildMu     sync.Mutex
	builtBinary string
	buildErr    error
)

// Cerberusd returns the path to a shared cerberusd test binary, building it at
// most once per test process. repoRoot must contain go.mod.
func Cerberusd(t *testing.T, repoRoot string) string {
	t.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	if buildErr != nil {
		t.Fatalf("cerberusd build previously failed: %v", buildErr)
	}
	if builtBinary != "" {
		if _, err := os.Stat(builtBinary); err == nil {
			return builtBinary
		}
	}
	binPath, err := buildCerberusd(repoRoot)
	if err != nil {
		buildErr = err
		t.Fatalf("build cerberusd: %v", err)
	}
	builtBinary = binPath
	t.Logf("[testdaemon] cerberusd ready at %s", binPath)
	return binPath
}

// BuildCerberusd builds (or reuses) the shared test binary without a *testing.T.
// Intended for non-test entrypoints such as go run ./test/e2e.
func BuildCerberusd(repoRoot string) (string, error) {
	buildMu.Lock()
	defer buildMu.Unlock()
	if builtBinary != "" {
		if _, err := os.Stat(builtBinary); err == nil {
			return builtBinary, nil
		}
	}
	return buildCerberusd(repoRoot)
}

func buildCerberusd(repoRoot string) (string, error) {
	binDir := filepath.Join(repoRoot, "test", ".testbin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir test bin dir: %w", err)
	}
	binPath := filepath.Join(binDir, executableName("cerberusd"))
	lockPath := filepath.Join(binDir, ".build.lock")
	release, err := acquireBuildLock(lockPath, 10*time.Minute)
	if err != nil {
		return "", err
	}
	defer release()

	// Always rebuild under the lock: reusing a binary left by a PREVIOUS run
	// silently tests stale code (go's own build cache makes a no-change rebuild
	// cheap, so the lock — not a stat shortcut — is what dedupes concurrent
	// builders). The in-process memo above still avoids rebuilding per test.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./cmd/cerberusd")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, out)
	}
	return binPath, nil
}

// acquireBuildLock creates lockPath exclusively; retries until timeout.
func acquireBuildLock(lockPath string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			return func() {
				_ = f.Close()
				_ = os.Remove(lockPath)
			}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("acquire build lock %s: %w", lockPath, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, fmt.Errorf("timeout acquiring build lock %s after %s", lockPath, timeout)
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
