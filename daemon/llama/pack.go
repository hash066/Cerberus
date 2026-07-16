package llama

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// A "llama pack" is the OFFICIAL upstream llama.cpp prebuilt release archive for
// this platform, fetched from ggml-org/llama.cpp at PinnedTag and verified against
// a sha256 compiled into this binary.
//
// # Why upstream prebuilts, and not a from-source build
//
// An earlier design assumed upstream's releases could not be used because
// ggml-rpc-server is gated behind -DGGML_RPC=ON at compile time and was presumed
// off in release builds. THAT ASSUMPTION WAS WRONG. Upstream's release workflow
// sets it for every job, workflow-wide:
//
//	# .github/workflows/release.yml @ 33a75f41c (tag b10021), line 33:
//	CMAKE_ARGS: "-DLLAMA_BUILD_EXAMPLES=OFF -DLLAMA_BUILD_TESTS=OFF \
//	             -DLLAMA_BUILD_TOOLS=ON -DLLAMA_BUILD_SERVER=ON -DGGML_RPC=ON"
//
// Every asset recorded in packs below was downloaded and inspected: each one
// contains BOTH llama-server and ggml-rpc-server, from one build, which is exactly
// what the version gate in locate.go needs (ggml-rpc-server has no --version flag,
// so its build number is read from the llama-server beside it).
//
// So Cerberus ships upstream's binaries rather than rebuilding them. Building from
// source would have meant a vendored submodule and a Vulkan SDK in CI to reproduce,
// bit for bit, an artifact upstream already publishes and signs off on.
//
// # The pack is not bundled in the installer
//
// Vulkan is ~33MB (against ~640MB for CUDA) and covers NVIDIA + AMD + Intel with one
// artifact, but that is still too much to force on every user who never runs a
// model. It is fetched on first use, with progress, and verified before extraction.

// Backend names a pack's compute backend.
type Backend string

const (
	// BackendVulkan covers NVIDIA + AMD + Intel in ~33MB.
	BackendVulkan Backend = "vulkan"
	// BackendMetal is macOS on Apple Silicon.
	BackendMetal Backend = "metal"
	// BackendCPU is the portable fallback.
	BackendCPU Backend = "cpu"
)

// archiveKind is how a pack is packaged. Upstream is not uniform: Windows assets
// are flat zips, every other platform is a tarball with a leading directory.
type archiveKind int

const (
	archiveZip archiveKind = iota
	archiveTarGz
)

// packAsset is one upstream release asset, pinned by digest.
//
// Name/SHA256/Size are kept together deliberately: they describe one file and must
// never drift apart. A digest that does not match its name is a brick.
type packAsset struct {
	// Name is the asset filename within the upstream release for PinnedTag.
	Name string
	// SHA256 is the digest of that exact asset. See the trust-anchor note on packs.
	SHA256 string
	// Size is the asset's length in bytes; checked before and after download so a
	// truncated or substituted body fails early with a clear message.
	Size int64
	// Kind is how to unpack it.
	Kind archiveKind
	// Strip is the number of leading path components to drop when extracting.
	// Upstream's tarballs nest everything under "llama-<tag>/"; the Windows zips
	// are flat. Locate() looks for the binaries directly in PackDir, so the prefix
	// has to come off or nothing is ever found.
	Strip int
}

// packs maps "<goos>/<goarch>/<backend>" to the upstream asset that serves it.
//
// THIS MAP IS THE TRUST ANCHOR. A pack is executable code; fetching one without a
// pinned digest means executing whatever the network returned. FetchPack REFUSES
// any platform/backend absent from this map rather than downloading unverifiable
// code, and refuses any download whose digest does not match.
//
// Every entry below was verified on 2026-07-16 against release b10021 by:
//  1. listing the release via the GitHub API (api.github.com/repos/ggml-org/
//     llama.cpp/releases/tags/b10021), which reports each asset's size and its
//     sha256 in the `digest` field;
//  2. downloading each asset and hashing it locally (sha256sum) — the local hash
//     matched the API's digest for all nine;
//  3. listing each archive to confirm it actually contains ggml-rpc-server. A
//     verified digest for an asset WITHOUT the worker binary would be a pack that
//     downloads and verifies and then fails to run.
//
// Do not add an entry from documentation, inference, or an educated guess. Only
// from an asset you downloaded and hashed.
//
// Coverage note: upstream publishes no Vulkan build for windows/arm64, and its
// macos-x64 build ships no Metal backend (BLAS only) — so those map to CPU, and
// DefaultBackend reflects that rather than promising acceleration that the
// artifact cannot deliver.
var packs = map[string]packAsset{
	// ---- windows -----------------------------------------------------------
	"windows/amd64/vulkan": {
		Name:   "llama-" + PinnedTag + "-bin-win-vulkan-x64.zip",
		SHA256: "116a27e9da51e2fbb75b8ff111b394b68112bcd7e94a19379af9e5221ab98970",
		Size:   33055286,
		Kind:   archiveZip,
		Strip:  0,
	},
	"windows/amd64/cpu": {
		Name:   "llama-" + PinnedTag + "-bin-win-cpu-x64.zip",
		SHA256: "1f0bd47f958155e602514701da5bd8b7b8d38133aeaa23d2d5a3d59718b1f0ab",
		Size:   18272252,
		Kind:   archiveZip,
		Strip:  0,
	},
	"windows/arm64/cpu": {
		Name:   "llama-" + PinnedTag + "-bin-win-cpu-arm64.zip",
		SHA256: "9a2a5fc1379baf891725689e077b865594dc70c86f5faa5e69fc173cfdfeee55",
		Size:   12175011,
		Kind:   archiveZip,
		Strip:  0,
	},

	// ---- linux -------------------------------------------------------------
	"linux/amd64/vulkan": {
		Name:   "llama-" + PinnedTag + "-bin-ubuntu-vulkan-x64.tar.gz",
		SHA256: "5bbc68171646b8f99a512ff5a5c7133a96e94d9d887090c585c13878055b96ad",
		Size:   31300503,
		Kind:   archiveTarGz,
		Strip:  1,
	},
	"linux/amd64/cpu": {
		Name:   "llama-" + PinnedTag + "-bin-ubuntu-x64.tar.gz",
		SHA256: "dae58702827d6cd258d18554bb7f0e8aa68394f1b1787e90aa37d9a7f32e56f9",
		Size:   15875967,
		Kind:   archiveTarGz,
		Strip:  1,
	},
	"linux/arm64/vulkan": {
		Name:   "llama-" + PinnedTag + "-bin-ubuntu-vulkan-arm64.tar.gz",
		SHA256: "661b531e8c2482ee981040639804c16482405168fa9fc9d5fa0f8e6dc02c8a28",
		Size:   25538982,
		Kind:   archiveTarGz,
		Strip:  1,
	},
	"linux/arm64/cpu": {
		Name:   "llama-" + PinnedTag + "-bin-ubuntu-arm64.tar.gz",
		SHA256: "d4a4b58a6774f1e61f6300911d10a9f7eac9e1015dbf21af2043edacf4d74c71",
		Size:   12807508,
		Kind:   archiveTarGz,
		Strip:  1,
	},

	// ---- darwin ------------------------------------------------------------
	"darwin/arm64/metal": {
		Name:   "llama-" + PinnedTag + "-bin-macos-arm64.tar.gz",
		SHA256: "e57a7585bfe2445431095b674dc562c350b4216bb4ec0d54b3c63d347d34c27c",
		Size:   10773507,
		Kind:   archiveTarGz,
		Strip:  1,
	},
	// Intel Mac: upstream's macos-x64 archive carries libggml-blas but NO
	// libggml-metal, so this is honestly a CPU/BLAS pack, not a Metal one.
	"darwin/amd64/cpu": {
		Name:   "llama-" + PinnedTag + "-bin-macos-x64.tar.gz",
		SHA256: "fd08fb679c926f94443ff9210a373ebffb3618960b5043696a89824a05074606",
		Size:   11043180,
		Kind:   archiveTarGz,
		Strip:  1,
	},
}

func packKey(goos, goarch string, backend Backend) string {
	return goos + "/" + goarch + "/" + string(backend)
}

// PackAssetFor returns the pinned upstream asset for a platform/backend.
func PackAssetFor(goos, goarch string, backend Backend) (packAsset, bool) {
	a, ok := packs[packKey(goos, goarch, backend)]
	return a, ok
}

// PackName returns the upstream asset filename for a platform/backend, or "" if
// upstream publishes nothing usable for it at PinnedTag.
func PackName(goos, goarch string, backend Backend) string {
	a, ok := PackAssetFor(goos, goarch, backend)
	if !ok {
		return ""
	}
	return a.Name
}

// DefaultBackend picks the best backend that a REAL pinned asset exists for on the
// running platform. It does not promise acceleration upstream does not ship:
// windows/arm64 has no Vulkan asset and macos-x64 has no Metal, so both get CPU.
func DefaultBackend() Backend { return defaultBackendFor(runtime.GOOS, runtime.GOARCH) }

func defaultBackendFor(goos, goarch string) Backend {
	for _, b := range []Backend{BackendMetal, BackendVulkan, BackendCPU} {
		if _, ok := packs[packKey(goos, goarch, b)]; ok {
			return b
		}
	}
	// Nothing is published for this platform; return CPU so the caller's error
	// names a backend rather than an empty string.
	return BackendCPU
}

// PackBaseURL is where packs are fetched from: upstream's release for PinnedTag.
// Overridable for testing and for air-gapped mirrors — but note that the digest
// check applies to a mirror exactly as it does to upstream, so a mirror must serve
// the identical bytes.
const PackBaseURL = "https://github.com/ggml-org/llama.cpp/releases/download/" + PinnedTag + "/"

// EnvPackURL overrides the pack base URL.
const EnvPackURL = "CERBERUS_LLAMA_PACK_URL"

// PackDir is where packs are installed: <user cache>/cerberus/llama/<tag>/.
// Keyed by tag so a version bump does not silently reuse old binaries.
func PackDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "cerberus", "llama", PinnedTag), nil
}

// Progress reports download progress. total is -1 when the server sends no length.
type Progress func(downloaded, total int64)

// FetchPack downloads and verifies the upstream pack for this platform, then
// extracts it into PackDir and re-runs the version gate.
//
// The sha256 is checked against a digest compiled into this binary BEFORE anything
// is extracted or executed. A mismatch deletes the download and fails.
func FetchPack(ctx context.Context, backend Backend, progress Progress) (Binaries, error) {
	asset, ok := PackAssetFor(runtime.GOOS, runtime.GOARCH, backend)
	if !ok {
		return Binaries{}, fmt.Errorf(
			"llama: no pinned llama.cpp pack for %s/%s/%s at %s.\n"+
				"A pack is executable code, so Cerberus only fetches assets whose sha256 is compiled "+
				"in (see packs in daemon/llama/pack.go); it will not download an unverifiable binary "+
				"and run it. Upstream may simply not publish this combination — %s/%s has a pinned "+
				"pack for backend %q.\n"+
				"Otherwise: build llama.cpp yourself from %s and point %s and %s at the binaries.",
			runtime.GOOS, runtime.GOARCH, backend, PinnedTag,
			runtime.GOOS, runtime.GOARCH, defaultBackendFor(runtime.GOOS, runtime.GOARCH),
			PinnedTag, EnvServer, EnvRPCServer)
	}

	dir, err := PackDir()
	if err != nil {
		return Binaries{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Binaries{}, err
	}

	base := strings.TrimSpace(os.Getenv(EnvPackURL))
	if base == "" {
		base = PackBaseURL
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}

	tmp, err := os.CreateTemp(dir, ".pack-*.part")
	if err != nil {
		return Binaries{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	got, n, err := download(ctx, base+asset.Name, tmp, asset.Size, progress)
	_ = tmp.Close()
	if err != nil {
		return Binaries{}, err
	}
	if got != asset.SHA256 {
		return Binaries{}, fmt.Errorf(
			"llama: SHA256 MISMATCH for %s\n  want %s\n  got  %s\n"+
				"The download was discarded and nothing was extracted or executed. Either the "+
				"artifact was tampered with in transit, or the pinned digest in daemon/llama/pack.go "+
				"is stale relative to what %s now serves.",
			asset.Name, asset.SHA256, got, base)
	}
	if n != asset.Size {
		// Unreachable if the digest matched, but a size disagreement means the
		// pinned metadata is internally inconsistent and must not be trusted.
		return Binaries{}, fmt.Errorf("llama: %s verified by digest but its length is %d, not the pinned %d", asset.Name, n, asset.Size)
	}

	if err := extract(tmpName, dir, asset); err != nil {
		return Binaries{}, err
	}
	// Locate re-applies the version gate: a verified digest proves WHAT we
	// downloaded, not that it is new enough to be safe to run.
	return Locate(ctx)
}

// download streams url into w, hashing as it goes, and returns the hex sha256 and
// the number of bytes written.
func download(ctx context.Context, url string, w io.Writer, wantSize int64, progress Progress) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("llama: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("llama: fetch %s: HTTP %s", url, resp.Status)
	}
	// Refuse an obviously-wrong body before streaming tens of MB of it. The digest
	// is still the authority; this is just a faster, clearer failure.
	if wantSize > 0 && resp.ContentLength > 0 && resp.ContentLength != wantSize {
		return "", 0, fmt.Errorf("llama: fetch %s: server offers %d bytes but the pinned asset is %d — refusing",
			url, resp.ContentLength, wantSize)
	}

	h := sha256.New()
	total := resp.ContentLength
	if total <= 0 && wantSize > 0 {
		total = wantSize
	}
	var done int64
	buf := make([]byte, 128*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			// Never write more than the pinned size: a body that keeps going is not
			// the asset we pinned, and should not be allowed to fill the disk.
			if wantSize > 0 && done+int64(n) > wantSize {
				return "", 0, fmt.Errorf("llama: fetch %s: body exceeds the pinned size of %d bytes — refusing", url, wantSize)
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return "", 0, err
			}
			h.Write(buf[:n])
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", 0, rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), done, nil
}

// extract unpacks a verified pack into dest.
func extract(src, dest string, asset packAsset) error {
	switch asset.Kind {
	case archiveZip:
		return unzip(src, dest, asset.Strip)
	case archiveTarGz:
		return untarGz(src, dest, asset.Strip)
	default:
		return fmt.Errorf("llama: unknown archive kind for %s", asset.Name)
	}
}

// stripPrefix drops the first n path components from a slash-separated archive
// entry name. It returns ok=false when the entry is shallower than n (e.g. the
// leading directory entry itself), meaning "skip this entry".
func stripPrefix(name string, n int) (string, bool) {
	name = strings.TrimLeft(path.Clean(strings.ReplaceAll(name, `\`, "/")), "/")
	if n <= 0 {
		return name, name != "" && name != "."
	}
	parts := strings.Split(name, "/")
	if len(parts) <= n {
		return "", false
	}
	return strings.Join(parts[n:], "/"), true
}

// safeTarget joins an archive entry to dest and refuses anything that escapes it
// (zip-slip / tar-slip).
func safeTarget(dest, name string) (string, error) {
	target := filepath.Join(dest, filepath.FromSlash(name))
	rel, err := filepath.Rel(dest, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("llama: pack entry %q escapes the destination directory", name)
	}
	return target, nil
}

// maxEntry bounds one extracted file: an entry claiming a huge size should not
// fill the disk. The largest real entry at b10021 is ggml-vulkan.dll at ~49MB.
const maxEntry = 2 << 30 // 2 GiB

// unzip extracts a zip pack. Upstream's Windows zips are flat and contain no
// symlinks, so a symlink here is unexpected and refused outright.
func unzip(src, dest string, strip int) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()

	for _, f := range zr.File {
		if f.FileInfo().Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("llama: pack contains a symlink (%s); refusing", f.Name)
		}
		name, ok := stripPrefix(f.Name, strip)
		if !ok {
			continue
		}
		target, err := safeTarget(dest, name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeZipEntry(f, target); err != nil {
			return err
		}
	}
	return nil
}

func writeZipEntry(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	mode := f.FileInfo().Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	return writeFile(target, rc, mode)
}

// untarGz extracts a gzipped tarball pack.
//
// Unlike the Windows zips, upstream's tarballs DO contain symlinks, and they are
// load-bearing: the shared libraries ship as soname chains
// (libggml.so -> libggml.so.0 -> libggml.so.0.16.0), and llama-server will not
// start if they are dropped. So symlinks are recreated rather than refused — but
// only when they stay inside the pack directory. An absolute target, or one that
// climbs out with .., is refused: that is the tar-slip a malicious archive would
// use to plant a link at an arbitrary path.
func untarGz(src, dest string, strip int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("llama: pack is not valid gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("llama: reading pack: %w", err)
		}

		name, ok := stripPrefix(hdr.Name, strip)
		if !ok {
			continue
		}
		target, err := safeTarget(dest, name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := hdr.FileInfo().Mode().Perm()
			if mode == 0 {
				mode = 0o644
			}
			if err := writeFile(target, tr, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeSymlink(dest, target, hdr.Linkname, name); err != nil {
				return err
			}
		case tar.TypeLink:
			// Hard links are not used by upstream's packs; refuse rather than
			// grow a second way to materialise a path.
			return fmt.Errorf("llama: pack entry %q is a hard link; refusing", hdr.Name)
		default:
			// Skip anything exotic (devices, fifos) instead of materialising it.
			continue
		}
	}
	return nil
}

// writeSymlink recreates a symlink, refusing any target that leaves dest.
func writeSymlink(dest, target, linkname, entry string) error {
	if linkname == "" {
		return fmt.Errorf("llama: pack entry %q is a symlink with an empty target; refusing", entry)
	}
	if filepath.IsAbs(linkname) || strings.HasPrefix(linkname, "/") {
		return fmt.Errorf("llama: pack entry %q symlinks to the absolute path %q; refusing", entry, linkname)
	}
	// Resolve the link relative to its own directory and require the result to
	// stay inside dest.
	resolved := filepath.Join(filepath.Dir(target), filepath.FromSlash(linkname))
	rel, err := filepath.Rel(dest, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("llama: pack entry %q symlinks to %q, which escapes the pack directory; refusing", entry, linkname)
	}
	// Replace an existing link so a re-extract is idempotent.
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(filepath.FromSlash(linkname), target); err != nil {
		return fmt.Errorf("llama: creating symlink %s -> %s: %w", target, linkname, err)
	}
	return nil
}

// writeFile writes one extracted entry, bounded by maxEntry.
func writeFile(target string, r io.Reader, mode os.FileMode) error {
	// Remove first: the target may exist as a symlink from an earlier extract, and
	// opening it would write THROUGH the link.
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, io.LimitReader(r, maxEntry)); err != nil {
		return err
	}
	return nil
}
