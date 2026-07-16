package audio

// osdispatch_test.go locks down the OS-backend build matrix: which os_*.go file
// each build configuration actually selects, and whether every backend still
// compiles for its platform.
//
// # Why this exists as a test rather than a one-off check someone ran once
//
// This package picks its backend purely through //go:build constraints across
// five files (os_windows.go, os_linux.go, os_darwin.go, os_darwin_stub.go,
// os_other.go). That dispatch is invisible at code-review time: nothing in the
// Go source says "darwin+cgo without the tag gets the stub", and no compiler
// error fires if a constraint is wrong — the build silently picks a DIFFERENT
// file and still succeeds. A wrong constraint therefore does not look like a
// bug, it looks like working code, until someone on that platform gets a stub
// where they expected a real microphone (or, worse, the reverse: an unverified
// backend promoted into a default build). Silent wrong-file selection is the
// exact class of bug that bit the mount lane.
//
// The checks below need no audio hardware and no C toolchain, so unlike the
// live-device tests in os_windows_test.go / os_linux_test.go they run
// everywhere, on every OS, including headless CI.
//
// # The one combination this file cannot compile-check
//
// darwin + cgo + cerberus_coreaudio (os_darwin.go, the CoreAudio backend) is
// the only configuration here that needs a real C toolchain and the macOS SDK,
// so it cannot be built from a non-Mac host. TestOSDispatchMatrix still pins
// which FILES it selects — `go list` resolves build constraints without
// invoking the C compiler — but proving that file COMPILES requires a macOS
// runner in CI. See os_darwin_stub.go for the promotion steps.

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// goListOSFiles asks the Go toolchain which os_*.go files it would select for
// the given build configuration, without building anything.
//
// It reads BOTH .GoFiles and .CgoFiles, which matters more than it looks: a
// file whose preamble imports "C" is reported in .CgoFiles and is ABSENT from
// .GoFiles. A matrix probe that only consulted .GoFiles would see os_darwin.go
// vanish under `CGO_ENABLED=1 -tags cerberus_coreaudio` and conclude the
// constraint was broken, when the build is in fact correct. (This is not
// hypothetical — the first version of this very check made that mistake.)
func goListOSFiles(t *testing.T, goos, goarch, cgo, tags string) []string {
	t.Helper()

	args := []string{"list", "-f", "{{range .GoFiles}}{{.}}\n{{end}}{{range .CgoFiles}}{{.}}\n{{end}}"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, ".")

	cmd := exec.Command("go", args...)
	cmd.Env = append(cmd.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED="+cgo)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list GOOS=%s GOARCH=%s CGO_ENABLED=%s tags=%q: %v\n%s",
			goos, goarch, cgo, tags, err, out)
	}

	var got []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "os_") {
			got = append(got, line)
		}
	}
	return got
}

// requireGoTool skips a test that shells out to the Go toolchain when no `go`
// binary is reachable — e.g. a test binary run standalone on a machine with no
// Go install (which is how this package's Linux backend gets exercised against
// WSLg's PulseAudio: cross-compile on the host, execute in the guest). The
// dispatch matrix is a property of the SOURCE, so it is fully checked wherever
// a toolchain does exist; skipping there costs no coverage.
func requireGoTool(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("shells out to the Go toolchain; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no go toolchain on PATH: %v", err)
	}
}

// TestOSDispatchMatrix pins every (GOOS, CGO_ENABLED, tags) -> os_*.go mapping
// this package intends. Each row is a promise about which backend a given build
// gets; a constraint edit that breaks a promise fails here loudly instead of
// silently shipping the wrong backend.
func TestOSDispatchMatrix(t *testing.T) {
	requireGoTool(t)

	cases := []struct {
		goos, goarch, cgo, tags string
		want                    []string
		why                     string
	}{
		// Windows: WASAPI via go-wca. Pure-Go COM over syscall, so cgo is
		// irrelevant to the selection and both settings must agree.
		{"windows", "amd64", "0", "", []string{"os_windows.go"}, "WASAPI is pure Go; cgo must not change the backend"},
		{"windows", "amd64", "1", "", []string{"os_windows.go"}, "WASAPI is pure Go; cgo must not change the backend"},

		// Linux: PulseAudio native protocol via jfreymuth/pulse. Also pure Go
		// (it speaks the wire protocol over a unix socket rather than linking
		// libpulse), which is what keeps the CGO_ENABLED=0 release
		// cross-compile working. Both cgo settings must select it.
		{"linux", "amd64", "0", "", []string{"os_linux.go"}, "the Pulse backend is pure Go; CGO_ENABLED=0 release builds must still get it"},
		{"linux", "amd64", "1", "", []string{"os_linux.go"}, "the Pulse backend is pure Go; cgo must not change the backend"},

		// macOS: the CoreAudio backend is opt-in behind BOTH cgo and the
		// cerberus_coreaudio tag. Every weaker combination must fall back to
		// the honest stub. The CGO_ENABLED=0 rows are what keep
		// build/release.sh's darwin cross-compile green.
		{"darwin", "arm64", "0", "", []string{"os_darwin_stub.go"}, "default darwin build must get the honest stub, never the unverified backend"},
		{"darwin", "arm64", "1", "", []string{"os_darwin_stub.go"}, "cgo alone must NOT be enough to opt into the never-compiled backend; a plain `go build` on a Mac (where cgo is on by default) must get the stub"},
		{"darwin", "arm64", "0", "cerberus_coreaudio", []string{"os_darwin_stub.go"}, "the tag alone must not be enough either: CoreAudio needs cgo, and a tag-without-cgo build must degrade to the stub rather than fail to build"},
		{"darwin", "amd64", "0", "", []string{"os_darwin_stub.go"}, "darwin/amd64 release cross-compile must get the stub"},

		// Both opt-ins present: the real (never-compiled) CoreAudio backend.
		// Reported in .CgoFiles, not .GoFiles — see goListOSFiles.
		{"darwin", "arm64", "1", "cerberus_coreaudio", []string{"os_darwin.go", "os_darwin_export.go"}, "cgo AND the tag together select the real CoreAudio backend"},
		{"darwin", "amd64", "1", "cerberus_coreaudio", []string{"os_darwin.go", "os_darwin_export.go"}, "cgo AND the tag together select the real CoreAudio backend"},

		// Everything else falls to the fallback stub. os_other.go's constraint
		// is `!windows && !darwin && !linux`; these rows are what prove the
		// negation actually covers the field rather than accidentally
		// swallowing (or missing) a platform that has a real backend.
		{"freebsd", "amd64", "0", "", []string{"os_other.go"}, "a platform with no backend gets the fallback stub"},
		{"openbsd", "amd64", "0", "", []string{"os_other.go"}, "a platform with no backend gets the fallback stub"},
		{"netbsd", "amd64", "0", "", []string{"os_other.go"}, "a platform with no backend gets the fallback stub"},
		{"solaris", "amd64", "0", "", []string{"os_other.go"}, "a platform with no backend gets the fallback stub"},
		{"plan9", "amd64", "0", "", []string{"os_other.go"}, "a platform with no backend gets the fallback stub"},
		{"js", "wasm", "0", "", []string{"os_other.go"}, "a platform with no backend gets the fallback stub"},
	}

	for _, tc := range cases {
		name := tc.goos + "_" + tc.goarch + "_cgo" + tc.cgo
		if tc.tags != "" {
			name += "_" + tc.tags
		}
		t.Run(name, func(t *testing.T) {
			got := goListOSFiles(t, tc.goos, tc.goarch, tc.cgo, tc.tags)
			if !equalStringSets(got, tc.want) {
				t.Errorf("GOOS=%s GOARCH=%s CGO_ENABLED=%s tags=%q selected %v, want %v\nwhy this matters: %s",
					tc.goos, tc.goarch, tc.cgo, tc.tags, got, tc.want, tc.why)
			}
		})
	}
}

// TestExactlyOneBackendPerPlatform is the structural invariant behind the table
// above: the os_*.go constraints must partition the platform space. Selecting
// two backends is a duplicate-symbol build failure; selecting none is a
// missing-function build failure. Both are caught by the table's exact-match
// assertions, but stating the invariant separately means a NEW platform added
// to the matrix cannot quietly land in a gap — the count check fails before
// anyone has to reason about the constraint algebra.
func TestExactlyOneBackendPerPlatform(t *testing.T) {
	requireGoTool(t)

	// (goos, goarch, cgo, tags, expected file count). darwin+cgo+tag expects 2
	// because the CoreAudio backend is split across os_darwin.go and
	// os_darwin_export.go (cgo forbids //export in a file whose preamble holds
	// definitions — see os_darwin_export.go).
	platforms := []struct {
		goos, goarch, cgo, tags string
		wantCount               int
	}{
		{"windows", "amd64", "0", "", 1},
		{"windows", "amd64", "1", "", 1},
		{"linux", "amd64", "0", "", 1},
		{"linux", "amd64", "1", "", 1},
		{"darwin", "arm64", "0", "", 1},
		{"darwin", "arm64", "1", "", 1},
		{"darwin", "arm64", "1", "cerberus_coreaudio", 2},
		{"freebsd", "amd64", "0", "", 1},
		{"openbsd", "amd64", "0", "", 1},
		{"netbsd", "amd64", "0", "", 1},
		{"solaris", "amd64", "0", "", 1},
		{"plan9", "amd64", "0", "", 1},
		{"js", "wasm", "0", "", 1},
	}

	for _, p := range platforms {
		got := goListOSFiles(t, p.goos, p.goarch, p.cgo, p.tags)
		switch {
		case len(got) == 0:
			t.Errorf("GOOS=%s GOARCH=%s CGO_ENABLED=%s tags=%q selects NO os_*.go file: this platform has fallen through every build constraint and will fail to build with undefined NewOSCaptureSource/NewOSPlaybackSink/EnumerateEndpoints",
				p.goos, p.goarch, p.cgo, p.tags)
		case len(got) != p.wantCount:
			t.Errorf("GOOS=%s GOARCH=%s CGO_ENABLED=%s tags=%q selects %d os_*.go file(s) %v, want %d: overlapping build constraints mean duplicate definitions of the OS entry points",
				p.goos, p.goarch, p.cgo, p.tags, len(got), got, p.wantCount)
		}
	}
}

// TestOSBackendsCompileForEveryPlatform is the compile-rot guard: it type-checks
// this package — including its _test.go files — for every platform whose backend
// is pure Go. Without it, a backend for an OS nobody develops on can break and
// nobody learns until a release build fails: os_linux.go's live tests only ever
// run on Linux, and os_other_test.go's stub-contract tests only ever run on a
// BSD/plan9/wasm host, i.e. effectively nowhere.
//
// `go vet` is used rather than `go build` deliberately: vet type-checks the test
// files too, so a broken os_other_test.go or os_linux_test.go is caught here.
// `go build` would silently ignore them.
//
// darwin+cgo+cerberus_coreaudio is absent on purpose — it is the one
// configuration needing a C toolchain and the macOS SDK. Adding it here would
// make this test fail on every non-Mac. That combination's compile check belongs
// on a macOS CI runner; this test's job is to make sure everything that CAN be
// checked from any host IS.
func TestOSBackendsCompileForEveryPlatform(t *testing.T) {
	requireGoTool(t)

	targets := []struct{ goos, goarch, why string }{
		{"windows", "amd64", "the WASAPI backend"},
		{"linux", "amd64", "the PulseAudio backend (pure Go — this is what keeps CGO_ENABLED=0 release cross-compiles working)"},
		{"darwin", "arm64", "the macOS stub that every default Mac build gets"},
		{"darwin", "amd64", "the macOS stub on the Intel release target"},
		{"freebsd", "amd64", "the os_other.go fallback and its stub-contract tests, which run on no developer machine"},
		{"js", "wasm", "the os_other.go fallback on a non-unix platform"},
	}

	for _, tg := range targets {
		t.Run(tg.goos+"_"+tg.goarch, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command("go", "vet", ".")
			cmd.Env = append(cmd.Environ(), "GOOS="+tg.goos, "GOARCH="+tg.goarch, "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("GOOS=%s GOARCH=%s go vet failed — %s no longer compiles: %v\n%s",
					tg.goos, tg.goarch, tg.why, err, out)
			}
		})
	}
}

// TestHostBackendIsNotTheFallbackStub guards against the most damaging silent
// dispatch failure: a host that HAS a real backend quietly building the
// os_other.go fallback, which would turn a working microphone into
// ErrOSAudioUnavailable across the whole product with no build error anywhere.
//
// It runs on the host's own GOOS, so it is the one check here that needs no
// toolchain subprocess.
func TestHostBackendIsNotTheFallbackStub(t *testing.T) {
	switch runtime.GOOS {
	case "windows", "linux":
		// These platforms have real backends. Their constructors may still
		// fail (ErrNoAudioDevice on a machine with no sound card), but they
		// must never report the generic "no backend in this build" sentinel —
		// that would mean os_other.go was selected instead of the real file.
		// The per-platform tests assert this against live hardware; this
		// assertion is the cheap, hardware-free version that also documents
		// the invariant in one place.
		if _, err := NewOSCaptureSource(testFormat()); err == ErrOSAudioUnavailable {
			t.Fatalf("GOOS=%s has a real audio backend, but the build selected the os_other.go fallback stub", runtime.GOOS)
		}
	case "darwin":
		// Correct EITHER way, so there is nothing to assert: the default build
		// gets os_darwin_stub.go (ErrOSAudioUnavailable is right), while a
		// -tags cerberus_coreaudio build gets the real backend. Asserting
		// either outcome would fail a legitimate build of the other.
		t.Skip("darwin's expected result depends on the cerberus_coreaudio tag; TestOSDispatchMatrix covers both")
	default:
		// os_other_test.go already asserts the stub contract on these.
		t.Skipf("GOOS=%s has no real backend; os_other_test.go covers the stub contract", runtime.GOOS)
	}
}

// equalStringSets compares two file lists ignoring order — `go list` makes no
// ordering promise across toolchain versions.
func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}
