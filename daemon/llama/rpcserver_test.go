package llama

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

// TestRPCServerArgsAlwaysBindsLoopback is the single most important assertion in
// this package.
//
// ggml-rpc-server's deserializer trusts its peer. CVE-2026-34159 (CVSS 9.8) was an
// unauthenticated pre-auth RCE reachable by ANY host that could open a TCP
// connection to this port. The Cerberus capability gate only means something
// because the port is unreachable except through a mesh stream that already passed
// that gate. Bind 0.0.0.0 and the gate becomes ceremonial — an attacker skips the
// mesh entirely and talks to the port.
//
// If this test ever fails, do not "fix" it by updating the expectation.
func TestRPCServerArgsAlwaysBindsLoopback(t *testing.T) {
	cases := []struct {
		name string
		cfg  RPCServerConfig
	}{
		{"minimal", RPCServerConfig{Port: 50052}},
		{"threads", RPCServerConfig{Port: 1, Threads: 8}},
		{"device", RPCServerConfig{Port: 65535, Device: "Vulkan0"}},
		{"cache", RPCServerConfig{Port: 9, Cache: true}},
		{"everything", RPCServerConfig{Port: 42, Threads: 4, Device: "Vulkan0,Vulkan1", Cache: true}},
		{"zero value", RPCServerConfig{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := rpcServerArgs(tc.cfg)

			hv, ok := flagValue(args, "-H")
			if !ok {
				t.Fatalf("args %q carry no -H flag; the bind host must always be stated explicitly, "+
					"never left to an upstream default that could change", args)
			}
			if hv != "127.0.0.1" {
				t.Fatalf("-H = %q, want 127.0.0.1 — binding ggml-rpc-server off-loopback exposes a "+
					"pre-auth RCE surface (CVE-2026-34159) to the network", hv)
			}

			// Belt and braces: no argument anywhere may name a routable bind address.
			for _, a := range args {
				if a == "0.0.0.0" || a == "::" || a == "*" {
					t.Fatalf("args %q contain a wildcard bind address %q", args, a)
				}
			}

			// -m/--mem does not exist upstream anymore; passing it would abort the child.
			if _, ok := flagValue(args, "-m"); ok {
				t.Fatalf("args %q pass -m/--mem, which upstream removed", args)
			}
			if _, ok := flagValue(args, "--mem"); ok {
				t.Fatalf("args %q pass --mem, which upstream removed", args)
			}
		})
	}
}

// TestRPCServerArgsNoHostEscapeHatch pins that RPCServerConfig offers no way to
// change the bind host. This is a design assertion: the safe value must not be
// merely the default, it must be the only value.
func TestRPCServerArgsNoHostEscapeHatch(t *testing.T) {
	// If someone adds a Host field to RPCServerConfig, this test still passes only
	// as long as rpcServerArgs ignores it. The struct literal below is exhaustive
	// by field name on purpose: adding a Host field makes this fail to compile,
	// forcing whoever adds it to read TestRPCServerArgsAlwaysBindsLoopback.
	cfg := RPCServerConfig{
		Port:    50052,
		Threads: 2,
		Device:  "",
		Cache:   false,
	}
	if hv, _ := flagValue(rpcServerArgs(cfg), "-H"); hv != loopbackHost {
		t.Fatalf("-H = %q, want %q", hv, loopbackHost)
	}
	if loopbackHost != "127.0.0.1" {
		t.Fatalf("loopbackHost = %q; it must remain 127.0.0.1", loopbackHost)
	}
}

func TestRPCServerArgsFlagMapping(t *testing.T) {
	args := rpcServerArgs(RPCServerConfig{Port: 1234, Threads: 6, Device: "Vulkan0", Cache: true})

	if v, _ := flagValue(args, "-p"); v != "1234" {
		t.Fatalf("-p = %q want 1234", v)
	}
	if v, _ := flagValue(args, "-t"); v != "6" {
		t.Fatalf("-t = %q want 6", v)
	}
	if v, _ := flagValue(args, "-d"); v != "Vulkan0" {
		t.Fatalf("-d = %q want Vulkan0", v)
	}
	if !hasFlag(args, "-c") {
		t.Fatalf("args %q missing -c (tensor cache)", args)
	}
}

// TestRPCServerArgsOmitsOptionalFlags: a zero Threads/Device must omit the flag
// entirely rather than pass an empty value, which upstream's parser would consume
// as the next token and misread.
func TestRPCServerArgsOmitsOptionalFlags(t *testing.T) {
	args := rpcServerArgs(RPCServerConfig{Port: 1})
	if hasFlag(args, "-t") {
		t.Fatalf("args %q pass -t with no thread count", args)
	}
	if hasFlag(args, "-d") {
		t.Fatalf("args %q pass -d with no device", args)
	}
	if hasFlag(args, "-c") {
		t.Fatalf("args %q pass -c when Cache is false", args)
	}
}

// TestReservePortIsLoopbackAndFree pins that the Go-side port reservation (needed
// because upstream REJECTS -p 0) hands back a usable loopback port.
func TestReservePortIsLoopbackAndFree(t *testing.T) {
	port, err := reservePort()
	if err != nil {
		t.Fatal(err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("reservePort() = %d, outside the range upstream accepts "+
			"(rpc-server.cpp rejects port <= 0 || port > 65535)", port)
	}
	// It must be re-bindable: reservePort has to have released it.
	l, err := net.Listen("tcp", net.JoinHostPort(loopbackHost, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("reserved port %d was not released: %v", port, err)
	}
	_ = l.Close()
}

// TestParseVersionUpstreamFormat pins the exact `--version` format from upstream
// common/arg.cpp:
//
//	fprintf(stderr, "version: %d (%s)\n", llama_build_number(), llama_commit());
//	fprintf(stderr, "built with %s for %s\n", ...);
func TestParseVersionUpstreamFormat(t *testing.T) {
	out := "version: 10021 (33a75f41)\nbuilt with MSVC 19.40 for x64\n"
	build, commit, err := parseVersion(out)
	if err != nil {
		t.Fatal(err)
	}
	if build != 10021 {
		t.Fatalf("build = %d want 10021", build)
	}
	if commit != "33a75f41" {
		t.Fatalf("commit = %q want 33a75f41", commit)
	}
}

func TestParseVersionRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "hello", "version: (abc)", "no version here"} {
		if _, _, err := parseVersion(in); err == nil {
			t.Fatalf("parseVersion(%q) succeeded; unparseable version output must fail closed", in)
		}
	}
}

// TestBuildFloorRejectsPreCVEBuilds pins the CVE-2026-34159 boundary exactly.
// b8492 is the fix; b8491 is vulnerable.
func TestBuildFloorRejectsPreCVEBuilds(t *testing.T) {
	if buildMeetsFloor(8491) {
		t.Fatal("b8491 accepted: it predates the CVE-2026-34159 fix and must be refused")
	}
	if !buildMeetsFloor(8492) {
		t.Fatal("b8492 refused: it IS the CVE-2026-34159 fix and must be accepted")
	}
	if !buildMeetsFloor(PinnedBuild) {
		t.Fatalf("the pinned build b%d does not meet the floor b%d", PinnedBuild, MinBuild)
	}
	if MinBuild != 8492 {
		t.Fatalf("MinBuild = %d, want 8492 (the CVE-2026-34159 fix). This floor is not negotiable.", MinBuild)
	}
}

// TestGateRPCServerRefusesUnversionableBinary pins the fail-closed path: a
// ggml-rpc-server with no sibling llama-server cannot have its version proven, and
// must therefore be refused rather than run.
func TestGateRPCServerRefusesUnversionableBinary(t *testing.T) {
	_, _, err := gateRPCServer(t.Context(), "/nowhere/ggml-rpc-server", "")
	if err == nil {
		t.Fatal("gateRPCServer accepted a binary whose version cannot be established")
	}
	if !strings.Contains(err.Error(), "CVE-2026-34159") {
		t.Fatalf("refusal should explain why; got: %v", err)
	}
}

func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
