package llama

// Pinned upstream llama.cpp build. Everything in this package is compiled against
// and tested against exactly this build; the pack workflow
// (.github/workflows/llama-pack.yml) builds this tag and nothing else.
//
// Verified against the upstream git refs API at pin time:
//
//	refs/tags/b10021 -> 33a75f41c30052fd3d1c38e8ed2f86ee3c3f8fba (commit)
//	refs/tags/b8492  -> 39bf0d3c6a95803e0f41aaba069ffbee26721042 (commit, the CVE fix)
const (
	// PinnedTag is the llama.cpp release tag this package targets.
	PinnedTag = "b10021"

	// PinnedCommit is the commit refs/tags/b10021 resolves to. It is the commit
	// the fetched pack's binaries report alongside their build number (verified:
	// `llama-server --version` prints "version: 10021 (33a75f41c)" to stderr), so
	// it identifies exactly which upstream build is installed.
	//
	// There is NO third_party/llama.cpp submodule and there never was: an earlier
	// comment here referenced one, from a plan to build llama.cpp from source that
	// was abandoned once upstream's prebuilt releases turned out to ship
	// ggml-rpc-server after all. Packs are fetched and digest-verified (pack.go).
	PinnedCommit = "33a75f41c30052fd3d1c38e8ed2f86ee3c3f8fba"

	// PinnedBuild is PinnedTag as an integer, for comparison against the build
	// number a binary reports via `--version`.
	PinnedBuild = 10021

	// MinBuild is the OLDEST llama.cpp build this package will execute.
	//
	// b8492 is the fix for CVE-2026-34159 (CVSS 9.8): an unauthenticated pre-auth
	// RCE in ggml-rpc's deserialize_tensor(), where a tensor with buffer=0 skipped
	// all bounds validation, handing any host with TCP access to the port arbitrary
	// process read/write via crafted GRAPH_COMPUTE messages.
	//
	// This floor is NOT configurable. CERBERUS_LLAMA_RPC_SERVER can move which
	// binary we run; it cannot lower this number. See locate.go.
	MinBuild = 8492
)

// buildMeetsFloor reports whether a reported build number is safe to execute.
// Kept as a function so the comparison has exactly one definition and the tests
// pin its boundary.
func buildMeetsFloor(build int) bool { return build >= MinBuild }
