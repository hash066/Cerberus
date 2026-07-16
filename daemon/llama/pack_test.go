package llama

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// The pinned map is the trust anchor for code we execute. A malformed entry is not
// a cosmetic problem: a wrong-length or upper-case digest can never match, which
// turns a pack into one that can never be installed.
func TestPacksArePinnedAndWellFormed(t *testing.T) {
	if len(packs) == 0 {
		t.Fatal("packs is empty: FetchPack would refuse every platform")
	}
	seen := map[string]string{}
	for key, a := range packs {
		if !hex64.MatchString(a.SHA256) {
			t.Errorf("%s: SHA256 %q is not 64 lower-case hex chars", key, a.SHA256)
		}
		if a.Size <= 0 {
			t.Errorf("%s: Size must be positive, got %d", key, a.Size)
		}
		if !strings.Contains(a.Name, PinnedTag) {
			t.Errorf("%s: asset %q does not carry the pinned tag %s — the map and version.go disagree",
				key, a.Name, PinnedTag)
		}
		// Two platforms must never claim the same asset with different digests.
		if prev, ok := seen[a.Name]; ok && prev != a.SHA256 {
			t.Errorf("asset %s is pinned to two different digests (%s vs %s)", a.Name, prev, a.SHA256)
		}
		seen[a.Name] = a.SHA256

		// The archive kind must match the filename, or extraction picks the wrong
		// reader and fails on a verified download.
		switch {
		case strings.HasSuffix(a.Name, ".zip"):
			if a.Kind != archiveZip {
				t.Errorf("%s: %s is a .zip but Kind is not archiveZip", key, a.Name)
			}
			if a.Strip != 0 {
				t.Errorf("%s: upstream's zips are flat; Strip should be 0, got %d", key, a.Strip)
			}
		case strings.HasSuffix(a.Name, ".tar.gz"):
			if a.Kind != archiveTarGz {
				t.Errorf("%s: %s is a .tar.gz but Kind is not archiveTarGz", key, a.Name)
			}
			// Upstream nests everything under llama-<tag>/; failing to strip it
			// means Locate never finds the binaries.
			if a.Strip != 1 {
				t.Errorf("%s: upstream's tarballs nest under llama-%s/; Strip should be 1, got %d", key, PinnedTag, a.Strip)
			}
		default:
			t.Errorf("%s: asset %q has no recognised archive extension", key, a.Name)
		}
	}
}

// Every key must be parseable as goos/goarch/backend, so packKey and the map
// cannot drift.
func TestPackKeysAreWellFormed(t *testing.T) {
	for key := range packs {
		parts := strings.Split(key, "/")
		if len(parts) != 3 {
			t.Errorf("key %q is not goos/goarch/backend", key)
			continue
		}
		if got := packKey(parts[0], parts[1], Backend(parts[2])); got != key {
			t.Errorf("packKey round-trip: got %q want %q", got, key)
		}
	}
}

// DefaultBackend must name a backend that a real pinned asset exists for —
// otherwise the default path fails closed for a platform we actually support.
func TestDefaultBackendResolvesToARealAsset(t *testing.T) {
	platforms := map[string]Backend{
		"windows/amd64": BackendVulkan,
		"windows/arm64": BackendCPU, // upstream publishes no Vulkan build for win/arm64
		"linux/amd64":   BackendVulkan,
		"linux/arm64":   BackendVulkan,
		"darwin/arm64":  BackendMetal,
		"darwin/amd64":  BackendCPU, // upstream's macos-x64 ships BLAS, not Metal
	}
	for plat, want := range platforms {
		parts := strings.Split(plat, "/")
		got := defaultBackendFor(parts[0], parts[1])
		if got != want {
			t.Errorf("defaultBackendFor(%s) = %q, want %q", plat, got, want)
		}
		if _, ok := PackAssetFor(parts[0], parts[1], got); !ok {
			t.Errorf("defaultBackendFor(%s) returned %q but no asset is pinned for it", plat, got)
		}
	}
}

// A platform we publish nothing for must REFUSE, not download something anyway.
func TestPackAssetForUnknownPlatform(t *testing.T) {
	if _, ok := PackAssetFor("plan9", "mips", BackendVulkan); ok {
		t.Fatal("PackAssetFor returned an asset for plan9/mips")
	}
	if n := PackName("plan9", "mips", BackendVulkan); n != "" {
		t.Fatalf("PackName for an unpinned platform = %q, want empty", n)
	}
}

func TestStripPrefix(t *testing.T) {
	cases := []struct {
		name  string
		strip int
		want  string
		ok    bool
	}{
		{"ggml-rpc-server.exe", 0, "ggml-rpc-server.exe", true},
		{"llama-b10021/ggml-rpc-server", 1, "ggml-rpc-server", true},
		{"llama-b10021/sub/dir/x.so", 1, "sub/dir/x.so", true},
		{"llama-b10021", 1, "", false},  // the prefix dir entry itself: skip
		{"llama-b10021/", 1, "", false}, // ditto, with a trailing slash
		{"a/b", 2, "", false},
		{`win\style\path.dll`, 1, "style/path.dll", true},
	}
	for _, c := range cases {
		got, ok := stripPrefix(c.name, c.strip)
		if got != c.want || ok != c.ok {
			t.Errorf("stripPrefix(%q, %d) = (%q, %v), want (%q, %v)", c.name, c.strip, got, ok, c.want, c.ok)
		}
	}
}

func TestSafeTargetRejectsEscape(t *testing.T) {
	dest := t.TempDir()
	for _, bad := range []string{"../evil", "../../evil", "a/../../evil"} {
		if _, err := safeTarget(dest, bad); err == nil {
			t.Errorf("safeTarget(%q) accepted an entry that escapes the destination", bad)
		}
	}
	if _, err := safeTarget(dest, "sub/ok.txt"); err != nil {
		t.Errorf("safeTarget rejected a legitimate entry: %v", err)
	}
}

// ---- zip -------------------------------------------------------------------

func buildZip(t *testing.T, entries map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUnzipExtractsFlatEntries(t *testing.T) {
	src := buildZip(t, map[string]string{
		"ggml-rpc-server.exe": "rpc",
		"llama-server.exe":    "server",
	})
	dest := t.TempDir()
	if err := unzip(src, dest, 0); err != nil {
		t.Fatalf("unzip: %v", err)
	}
	for name, want := range map[string]string{"ggml-rpc-server.exe": "rpc", "llama-server.exe": "server"} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestUnzipRejectsZipSlip(t *testing.T) {
	src := buildZip(t, map[string]string{"../escaped.txt": "pwned"})
	dest := t.TempDir()
	err := unzip(src, dest, 0)
	if err == nil {
		t.Fatal("unzip accepted a zip-slip entry")
	}
	if !strings.Contains(err.Error(), "escapes the destination") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---- tar.gz ----------------------------------------------------------------

type tarEntry struct {
	name string
	body string
	link string // non-empty => symlink
	dir  bool
}

func buildTarGz(t *testing.T, entries []tarEntry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o755}
		switch {
		case e.dir:
			hdr.Typeflag, hdr.Mode = tar.TypeDir, 0o755
		case e.link != "":
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, e.link
		default:
			hdr.Typeflag, hdr.Size = tar.TypeReg, int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// Upstream's tarballs nest under llama-<tag>/ AND carry load-bearing soname
// symlinks (libggml.so -> libggml.so.0 -> libggml.so.0.16.0). Dropping either the
// strip or the links yields a pack that verifies and then cannot run.
func TestUntarGzStripsPrefixAndKeepsSonameSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows; upstream's Windows pack is a flat zip with no symlinks")
	}
	src := buildTarGz(t, []tarEntry{
		{name: "llama-" + PinnedTag, dir: true},
		{name: "llama-" + PinnedTag + "/ggml-rpc-server", body: "rpc"},
		{name: "llama-" + PinnedTag + "/libggml.so.0.16.0", body: "real"},
		{name: "llama-" + PinnedTag + "/libggml.so.0", link: "libggml.so.0.16.0"},
		{name: "llama-" + PinnedTag + "/libggml.so", link: "libggml.so.0"},
	})
	dest := t.TempDir()
	if err := untarGz(src, dest, 1); err != nil {
		t.Fatalf("untarGz: %v", err)
	}

	// The prefix must be gone: Locate looks in PackDir directly.
	if _, err := os.Stat(filepath.Join(dest, "llama-"+PinnedTag)); err == nil {
		t.Errorf("the llama-%s/ prefix survived extraction; Locate would not find the binaries", PinnedTag)
	}
	got, err := os.ReadFile(filepath.Join(dest, "ggml-rpc-server"))
	if err != nil || string(got) != "rpc" {
		t.Fatalf("ggml-rpc-server not extracted to the pack root: %v (%q)", err, got)
	}
	// The whole soname chain must resolve.
	got, err = os.ReadFile(filepath.Join(dest, "libggml.so"))
	if err != nil {
		t.Fatalf("libggml.so does not resolve: %v", err)
	}
	if string(got) != "real" {
		t.Errorf("libggml.so resolved to %q, want %q", got, "real")
	}
	fi, err := os.Lstat(filepath.Join(dest, "libggml.so"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("libggml.so was materialised as a regular file, not a symlink")
	}
}

// A symlink is the classic way a malicious archive plants a file outside the
// destination. These must be refused even though legitimate symlinks are allowed.
func TestUntarGzRejectsMaliciousSymlinks(t *testing.T) {
	cases := []struct {
		name string
		link string
	}{
		{"absolute target", "/etc/passwd"},
		{"parent escape", "../../../etc/passwd"},
		{"empty target", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// An empty Linkname is encoded as a regular file by archive/tar, so
			// exercise writeSymlink directly for that case.
			dest := t.TempDir()
			if c.link == "" {
				err := writeSymlink(dest, filepath.Join(dest, "x"), "", "x")
				if err == nil {
					t.Fatal("writeSymlink accepted an empty target")
				}
				return
			}
			src := buildTarGz(t, []tarEntry{
				{name: "llama-" + PinnedTag + "/evil", link: c.link},
			})
			err := untarGz(src, dest, 1)
			if err == nil {
				t.Fatalf("untarGz accepted a symlink to %q", c.link)
			}
			if !strings.Contains(err.Error(), "refusing") {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestUntarGzRejectsHardLinks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name: "llama-" + PinnedTag + "/link", Typeflag: tar.TypeLink, Linkname: "target", Mode: 0o644,
	}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gw.Close()
	_ = f.Close()

	if err := untarGz(p, t.TempDir(), 1); err == nil {
		t.Fatal("untarGz accepted a hard link")
	}
}

// ---- fetch -----------------------------------------------------------------

// redirectCacheDir points os.UserCacheDir at a temp dir so a test never writes to
// the real user cache.
func redirectCacheDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("LocalAppData", dir)
	case "darwin":
		t.Setenv("HOME", dir)
	default:
		t.Setenv("XDG_CACHE_HOME", dir)
	}
}

// The whole point of the pinned map: bytes that are not the bytes we pinned must
// never be extracted, let alone executed.
func TestFetchPackRefusesDigestMismatch(t *testing.T) {
	backend := DefaultBackend()
	asset, ok := PackAssetFor(runtime.GOOS, runtime.GOARCH, backend)
	if !ok {
		t.Skipf("no pinned pack for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	redirectCacheDir(t)

	// Serve a body of the pinned LENGTH but the wrong content, so the size guard
	// passes and the digest check is what has to catch it.
	body := bytes.Repeat([]byte("x"), int(asset.Size))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, asset.Name) {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	t.Setenv(EnvPackURL, srv.URL)

	_, err := FetchPack(context.Background(), backend, nil)
	if err == nil {
		t.Fatal("FetchPack accepted a body that does not match the pinned digest")
	}
	if !strings.Contains(err.Error(), "SHA256 MISMATCH") {
		t.Fatalf("expected a digest-mismatch refusal, got: %v", err)
	}

	// Nothing may survive a failed verification.
	dir, err := PackDir()
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".pack-") { // temp file is removed on return
				t.Errorf("a rejected pack left %s behind in the pack dir", e.Name())
			}
		}
	}
}

// A body longer than the pinned size must be cut off rather than allowed to fill
// the disk.
func TestFetchPackRefusesOversizedBody(t *testing.T) {
	backend := DefaultBackend()
	asset, ok := PackAssetFor(runtime.GOOS, runtime.GOARCH, backend)
	if !ok {
		t.Skipf("no pinned pack for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	redirectCacheDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Content-Length: force the streaming guard to be what stops this.
		w.Header().Set("Transfer-Encoding", "chunked")
		chunk := bytes.Repeat([]byte("y"), 1<<20)
		for written := int64(0); written < asset.Size+(2<<20); written += int64(len(chunk)) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	t.Setenv(EnvPackURL, srv.URL)

	_, err := FetchPack(context.Background(), backend, nil)
	if err == nil {
		t.Fatal("FetchPack accepted a body larger than the pinned size")
	}
	if !strings.Contains(err.Error(), "exceeds the pinned size") && !strings.Contains(err.Error(), "SHA256 MISMATCH") {
		t.Fatalf("expected an oversize/mismatch refusal, got: %v", err)
	}
}

// A wrong Content-Length should be refused before the body is streamed.
func TestFetchPackRefusesWrongContentLength(t *testing.T) {
	backend := DefaultBackend()
	asset, ok := PackAssetFor(runtime.GOOS, runtime.GOARCH, backend)
	if !ok {
		t.Skipf("no pinned pack for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	redirectCacheDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(asset.Size/2))
		_, _ = w.Write(bytes.Repeat([]byte("z"), int(asset.Size/2)))
	}))
	defer srv.Close()
	t.Setenv(EnvPackURL, srv.URL)

	_, err := FetchPack(context.Background(), backend, nil)
	if err == nil {
		t.Fatal("FetchPack accepted a body whose length disagrees with the pinned size")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("expected a length refusal, got: %v", err)
	}
}

// TestPinnedDigestsMatchUpstream re-checks every pinned digest against the live
// upstream release. Skipped unless CERBERUS_TEST_UPSTREAM=1, so the normal suite
// stays offline and hermetic; .github/workflows/llama-pack.yml runs it.
//
// This is drift detection. A GitHub release is mutable: assets can be deleted and
// re-uploaded, and a re-cut b10021 would leave every pinned digest here pointing
// at bytes that no longer exist. Because FetchPack is fail-closed, that does not
// produce a security hole — it produces a pack nobody can install. This test is
// how we find that out before users do.
func TestPinnedDigestsMatchUpstream(t *testing.T) {
	if os.Getenv("CERBERUS_TEST_UPSTREAM") != "1" {
		t.Skip("set CERBERUS_TEST_UPSTREAM=1 to check the pinned digests against the live upstream release")
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://api.github.com/repos/ggml-org/llama.cpp/releases/tags/"+PinnedTag, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// CI has a token; using it avoids the 60/hr unauthenticated rate limit.
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("querying the upstream release: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upstream release API returned HTTP %s", resp.Status)
	}

	var rel struct {
		TagName         string `json:"tag_name"`
		TargetCommitish string `json:"target_commitish"`
		Assets          []struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			Digest string `json:"digest"` // "sha256:<hex>"
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		t.Fatal(err)
	}

	// The tag must still resolve to the commit version.go pins.
	if rel.TargetCommitish != PinnedCommit {
		t.Errorf("upstream tag %s now points at %s, but version.go pins %s",
			PinnedTag, rel.TargetCommitish, PinnedCommit)
	}

	upstream := map[string]struct {
		size   int64
		digest string
	}{}
	for _, a := range rel.Assets {
		upstream[a.Name] = struct {
			size   int64
			digest string
		}{a.Size, strings.TrimPrefix(a.Digest, "sha256:")}
	}

	for key, a := range packs {
		u, ok := upstream[a.Name]
		if !ok {
			t.Errorf("%s: upstream release %s no longer contains %s — FetchPack would 404 for this platform",
				key, PinnedTag, a.Name)
			continue
		}
		if u.digest == "" {
			t.Logf("%s: upstream reports no digest for %s; size-only check", key, a.Name)
		} else if u.digest != a.SHA256 {
			t.Errorf("%s: DIGEST DRIFT for %s\n  pinned   %s\n  upstream %s\n"+
				"Upstream re-cut this asset. Every user on this platform now gets a fail-closed "+
				"refusal from FetchPack until the pin is updated.", key, a.Name, a.SHA256, u.digest)
		}
		if u.size != a.Size {
			t.Errorf("%s: %s is %d bytes upstream, pinned as %d", key, a.Name, u.size, a.Size)
		}
	}
}

// TestExtractRealUpstreamPack proves the whole non-network half of FetchPack
// against an ACTUAL upstream archive: verify the pinned digest, extract it, and
// run Locate's CVE version gate over the result.
//
// Skipped unless CERBERUS_TEST_PACK_ARCHIVE names a downloaded upstream asset, so
// CI stays hermetic:
//
//	go test ./daemon/llama -run TestExtractRealUpstreamPack \
//	  -args # CERBERUS_TEST_PACK_ARCHIVE=/path/to/llama-b10021-bin-win-vulkan-x64.zip
//
// This is the test that would have caught two real bugs: upstream's tarballs nest
// under llama-<tag>/ (so Locate finds nothing without Strip), and they carry
// load-bearing soname symlinks (so an extractor that refuses symlinks produces a
// pack that cannot start).
func TestExtractRealUpstreamPack(t *testing.T) {
	archive := strings.TrimSpace(os.Getenv("CERBERUS_TEST_PACK_ARCHIVE"))
	if archive == "" {
		t.Skip("set CERBERUS_TEST_PACK_ARCHIVE to a downloaded upstream asset to run this")
	}

	// The archive must be one we actually pinned, matched by filename.
	var asset packAsset
	var key string
	for k, a := range packs {
		if a.Name == filepath.Base(archive) {
			asset, key = a, k
			break
		}
	}
	if key == "" {
		t.Fatalf("%s is not a pinned asset; nothing to verify it against", filepath.Base(archive))
	}

	// Digest first: this is the check that makes extraction safe at all.
	sum, err := sha256File(archive)
	if err != nil {
		t.Fatal(err)
	}
	if sum != asset.SHA256 {
		t.Fatalf("%s does not match its pinned digest\n  want %s\n  got  %s\n"+
			"Either the download is corrupt or the pinned digest is wrong — the latter would "+
			"brick FetchPack for every user on %s.", asset.Name, asset.SHA256, sum, key)
	}
	if fi, err := os.Stat(archive); err == nil && fi.Size() != asset.Size {
		t.Fatalf("%s is %d bytes, pinned as %d", asset.Name, fi.Size(), asset.Size)
	}

	dest := t.TempDir()
	if err := extract(archive, dest, asset); err != nil {
		t.Fatalf("extract: %v", err)
	}

	// Both binaries must land at the pack ROOT — that is where Locate looks.
	for _, base := range []string{"ggml-rpc-server", "llama-server"} {
		p := filepath.Join(dest, exeName(base))
		if _, err := os.Stat(p); err != nil {
			entries, _ := os.ReadDir(dest)
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("%s is not at the pack root after extraction (Locate would miss it). Root contains: %v",
				exeName(base), names)
		}
	}

	// And the real version gate must accept it: this runs the actual binary.
	t.Setenv(EnvPackDir, dest)
	t.Setenv(EnvServer, "")
	t.Setenv(EnvRPCServer, "")
	bins, err := Locate(context.Background())
	if err != nil {
		t.Fatalf("Locate rejected a freshly extracted upstream pack: %v", err)
	}
	if bins.Build != PinnedBuild {
		t.Errorf("Locate reported build %d, want the pinned %d", bins.Build, PinnedBuild)
	}
	if !strings.HasPrefix(PinnedCommit, bins.Commit) {
		t.Errorf("Locate reported commit %q, which is not a prefix of the pinned %s", bins.Commit, PinnedCommit)
	}
	t.Logf("verified real pack %s: build b%d (%s) at %s", asset.Name, bins.Build, bins.Commit, bins.Dir)
}

// download() must report the true sha256 of what it wrote.
func TestDownloadHashesWhatItWrites(t *testing.T) {
	payload := []byte("the quick brown fox")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	got, n, err := download(context.Background(), srv.URL, &buf, int64(len(payload)), nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("download hash = %s, want %s", got, want)
	}
	if n != int64(len(payload)) {
		t.Errorf("download wrote %d bytes, want %d", n, len(payload))
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Error("download wrote the wrong bytes")
	}
}
