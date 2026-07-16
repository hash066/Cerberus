package llama

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Binaries is a located, VERSION-GATED pair of llama.cpp executables. A non-zero
// Binaries value has already passed the CVE floor check — construct it only via
// Locate.
type Binaries struct {
	// Server is the absolute path to llama-server.
	Server string
	// RPCServer is the absolute path to ggml-rpc-server. NOTE the name: upstream's
	// tools/rpc/CMakeLists.txt does `set(TARGET ggml-rpc-server)` — it is NOT
	// "rpc-server", which is the historical name from examples/rpc/.
	//
	// Upstream's prebuilt release archives DO contain it. An earlier version of
	// this comment claimed the opposite and drove a plan to vendor and build
	// llama.cpp from source; that was wrong, and it was checked three ways before
	// being corrected: upstream's release.yml sets -DGGML_RPC=ON in its
	// workflow-level CMAKE_ARGS, all nine published assets contain the binary, and
	// it runs. So Cerberus fetches the official pack (pack.go) rather than
	// building one.
	RPCServer string
	// Build is the build number both binaries reported (e.g. 10021).
	Build int
	// Commit is the short commit hash reported alongside Build.
	Commit string
	// Dir is the directory the binaries were found in.
	Dir string
}

// Env vars. The path overrides exist for development against a locally built
// llama.cpp. They move WHICH binary runs; they cannot lower the version floor.
const (
	// EnvRPCServer overrides the ggml-rpc-server path.
	EnvRPCServer = "CERBERUS_LLAMA_RPC_SERVER"
	// EnvServer overrides the llama-server path.
	EnvServer = "CERBERUS_LLAMA_SERVER"
	// EnvPackDir overrides the directory the pack is looked up in.
	EnvPackDir = "CERBERUS_LLAMA_PACK_DIR"
)

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// versionRe matches llama.cpp's `--version` output. common/arg.cpp prints:
//
//	fprintf(stderr, "version: %d (%s)\n", llama_build_number(), llama_commit());
//
// Note it goes to STDERR, not stdout — a reader that only watches stdout sees
// nothing and would wrongly conclude the binary is ancient.
var versionRe = regexp.MustCompile(`version:\s+(\d+)\s+\(([0-9a-fA-F]+)\)`)

// parseVersion extracts the build number and commit from `--version` output.
// Exported behaviour is pinned by locate_test.go against the real upstream format.
func parseVersion(out string) (int, string, error) {
	m := versionRe.FindStringSubmatch(out)
	if m == nil {
		return 0, "", fmt.Errorf("llama: could not find a build number in --version output (want %q); got: %s",
			"version: <N> (<commit>)", truncate(strings.TrimSpace(out), 200))
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, "", fmt.Errorf("llama: unparseable build number %q: %w", m[1], err)
	}
	return n, m[2], nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// probeBuild runs `<bin> --version` and returns the reported build number.
//
// IMPORTANT: this only works for binaries built from common/arg.cpp — llama-server
// and llama-cli. ggml-rpc-server has its OWN minimal argument parser (tools/rpc/
// rpc-server.cpp) with NO --version flag and no version output whatsoever, so it
// cannot self-report. See gateRPCServer for how its version is established.
func probeBuild(ctx context.Context, bin string) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "--version")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// `--version` exits 0 via exit(0) in the arg handler, but tolerate a non-zero
	// status as long as the output is parseable — the version string is the
	// contract here, not the exit code.
	runErr := cmd.Run()

	// stderr first: that is where upstream prints it. Fall back to stdout in case a
	// future upstream moves it.
	combined := stderr.String() + "\n" + stdout.String()
	build, commit, err := parseVersion(combined)
	if err != nil {
		if runErr != nil {
			return 0, "", fmt.Errorf("llama: %s --version failed: %w (output: %s)", filepath.Base(bin), runErr, truncate(strings.TrimSpace(combined), 200))
		}
		return 0, "", err
	}
	return build, commit, nil
}

// gateRPCServer establishes the build number for a ggml-rpc-server and REFUSES it
// if it predates the CVE-2026-34159 fix.
//
// ggml-rpc-server cannot report its own version (no --version flag upstream), so
// the build number is taken from the llama-server sitting BESIDE it. That is sound
// for a Cerberus pack, where both binaries are produced by one CI job from one
// pinned tag, and it is the reason a bare ggml-rpc-server path with no sibling is
// REFUSED rather than trusted:
//
// Failing closed here is the entire point. The alternative — "we could not
// determine the version, so proceed" — is precisely how a pre-b8492 binary with a
// 9.8-severity unauthenticated RCE ends up listening on a port.
func gateRPCServer(ctx context.Context, rpcServer, server string) (int, string, error) {
	if server == "" {
		return 0, "", fmt.Errorf(
			"llama: refusing to run %s: its version cannot be established.\n"+
				"ggml-rpc-server has no --version flag, so Cerberus reads the build number from the "+
				"llama-server built alongside it, and no llama-server was found in %s.\n"+
				"Builds before b%d contain CVE-2026-34159, an unauthenticated remote-code-execution "+
				"bug in the RPC deserializer (CVSS 9.8). Cerberus will not execute a binary it cannot "+
				"prove is patched.\n"+
				"Fix: use a Cerberus llama pack (both binaries together), or set %s to a llama-server "+
				"from the same build.",
			filepath.Base(rpcServer), filepath.Dir(rpcServer), MinBuild, EnvServer)
	}
	build, commit, err := probeBuild(ctx, server)
	if err != nil {
		return 0, "", err
	}
	if !buildMeetsFloor(build) {
		return 0, "", fmt.Errorf(
			"llama: REFUSING to run llama.cpp build b%d — it is older than b%d and contains "+
				"CVE-2026-34159, an unauthenticated pre-auth RCE in the ggml-rpc deserializer "+
				"(CVSS 9.8: a tensor with buffer=0 skips bounds validation, giving any peer that "+
				"reaches the port arbitrary process read/write).\n"+
				"This floor is not configurable. Upgrade to b%d or newer (Cerberus pins b%s).",
			build, MinBuild, MinBuild, PinnedTag)
	}
	return build, commit, nil
}

// candidateDirs returns the directories searched for a llama pack, in order.
func candidateDirs() []string {
	var dirs []string
	if d := strings.TrimSpace(os.Getenv(EnvPackDir)); d != "" {
		dirs = append(dirs, d)
	}
	if d, err := PackDir(); err == nil {
		dirs = append(dirs, d)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	return dirs
}

// findBinary resolves one binary: explicit env override, then the pack dirs, then
// PATH.
func findBinary(envKey, base string) string {
	if p := strings.TrimSpace(os.Getenv(envKey)); p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
				return abs
			}
		}
		// An override that points at nothing is a hard miss, not a reason to go
		// hunting elsewhere: silently running a DIFFERENT binary than the operator
		// named is worse than failing.
		return ""
	}
	for _, dir := range candidateDirs() {
		p := filepath.Join(dir, exeName(base))
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath(exeName(base)); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	return ""
}

// Locate finds the llama.cpp binaries and version-gates them. Every caller in this
// package goes through here; there is no path that spawns a llama.cpp binary
// without passing the CVE floor.
//
// A missing pack is reported as ErrPackMissing so callers can offer to fetch one
// (see pack.go) rather than treating it as a hard failure.
func Locate(ctx context.Context) (Binaries, error) {
	rpcServer := findBinary(EnvRPCServer, "ggml-rpc-server")
	server := findBinary(EnvServer, "llama-server")

	if rpcServer == "" && server == "" {
		return Binaries{}, fmt.Errorf("%w: no llama.cpp pack found (looked in %s, and PATH)",
			ErrPackMissing, strings.Join(candidateDirs(), ", "))
	}
	if rpcServer == "" {
		return Binaries{}, fmt.Errorf("%w: found llama-server at %s but no %s beside it — "+
			"an upstream llama.cpp release archive contains both, so this looks like a "+
			"partial or hand-assembled directory; run `cerberus llama fetch` to install a "+
			"complete, digest-verified pack",
			ErrPackMissing, server, exeName("ggml-rpc-server"))
	}

	build, commit, err := gateRPCServer(ctx, rpcServer, server)
	if err != nil {
		return Binaries{}, err
	}
	return Binaries{
		Server:    server,
		RPCServer: rpcServer,
		Build:     build,
		Commit:    commit,
		Dir:       filepath.Dir(rpcServer),
	}, nil
}
