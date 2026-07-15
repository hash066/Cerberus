package llama

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// A "llama pack" is a zip of llama.cpp binaries built by
// .github/workflows/llama-pack.yml from the pinned tag, named:
//
//	cerberus-llama-<os>-<arch>-<backend>-<tag>.zip
//
// It is NOT bundled in the base installer. Vulkan is ~33MB against ~640MB for
// CUDA, and it covers NVIDIA + AMD + Intel with one artifact, but that is still
// too much to force on every user who never runs a model. It is fetched on first
// use, with progress, and verified against a sha256 compiled into this binary.
//
// Upstream's own prebuilt releases are NOT an option: GGML_RPC is compile-time and
// off in release builds, so they contain no ggml-rpc-server at all. The pack must
// be built from source.

// Backend names a pack's compute backend.
type Backend string

const (
	// BackendVulkan covers NVIDIA + AMD + Intel in ~33MB.
	BackendVulkan Backend = "vulkan"
	// BackendMetal is macOS/Apple Silicon.
	BackendMetal Backend = "metal"
	// BackendCPU is the portable fallback.
	BackendCPU Backend = "cpu"
)

// PackName returns the artifact filename for a platform/backend at the pinned tag.
func PackName(goos, goarch string, backend Backend) string {
	return fmt.Sprintf("cerberus-llama-%s-%s-%s-%s.zip", goos, goarch, backend, PinnedTag)
}

// DefaultBackend picks the backend for the running platform.
func DefaultBackend() Backend {
	if runtime.GOOS == "darwin" {
		return BackendMetal
	}
	return BackendVulkan
}

// packSHA256 maps a pack filename to its expected sha256.
//
// THESE ARE PLACEHOLDERS AND THE FETCHER REFUSES TO RUN WITHOUT THEM.
//
// They cannot be filled in by hand or guessed: the only honest source is the
// digest of an artifact that .github/workflows/llama-pack.yml actually produced.
// The workflow emits a .sha256 next to each zip; the release step that publishes a
// pack must paste those digests here, and that commit is what makes fetching work.
//
// Until then FetchPack fails with a message saying exactly this, rather than
// downloading an unverifiable binary and executing it. An unverified llama pack is
// an arbitrary-code-execution vector: this map is the trust anchor.
var packSHA256 = map[string]string{}

// PackBaseURL is where packs are fetched from. Overridable for testing and for
// air-gapped mirrors.
const PackBaseURL = "https://github.com/hash066/cerberus/releases/download/llama-pack-" + PinnedTag + "/"

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

// Progress reports download progress.
type Progress func(downloaded, total int64)

// FetchPack downloads and verifies the pack for this platform, then extracts it
// into PackDir. It is a no-op if the binaries are already present and gated.
//
// The sha256 is checked against a digest compiled into this binary BEFORE anything
// is extracted or executed. A mismatch deletes the download and fails.
func FetchPack(ctx context.Context, backend Backend, progress Progress) (Binaries, error) {
	name := PackName(runtime.GOOS, runtime.GOARCH, backend)

	want, ok := packSHA256[name]
	if !ok || want == "" {
		return Binaries{}, fmt.Errorf(
			"llama: cannot fetch %s — no sha256 is compiled in for it.\n"+
				"A llama pack is executable code; fetching one without a pinned digest would mean "+
				"executing whatever the network returned. The digest must come from an artifact "+
				"that .github/workflows/llama-pack.yml actually built (it emits a .sha256 beside "+
				"each zip) and be recorded in packSHA256 in daemon/llama/pack.go.\n"+
				"Until then, build llama.cpp yourself from the pinned tag %s and point %s and %s at "+
				"the binaries.",
			name, PinnedTag, EnvServer, EnvRPCServer)
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

	tmp, err := os.CreateTemp(dir, ".pack-*.zip")
	if err != nil {
		return Binaries{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	got, err := download(ctx, base+name, tmp, progress)
	_ = tmp.Close()
	if err != nil {
		return Binaries{}, err
	}
	if got != want {
		return Binaries{}, fmt.Errorf(
			"llama: SHA256 MISMATCH for %s\n  want %s\n  got  %s\n"+
				"The download was discarded and nothing was executed. Either the artifact was "+
				"tampered with in transit, or packSHA256 is stale relative to the published pack.",
			name, want, got)
	}
	if err := unzip(tmpName, dir); err != nil {
		return Binaries{}, err
	}
	// Locate re-applies the version gate: a verified digest proves WHAT we
	// downloaded, not that it is new enough to be safe to run.
	return Locate(ctx)
}

func download(ctx context.Context, url string, w io.Writer, progress Progress) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("llama: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llama: fetch %s: HTTP %s", url, resp.Status)
	}

	h := sha256.New()
	total := resp.ContentLength
	var done int64
	buf := make([]byte, 128*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return "", err
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
			return "", rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// unzip extracts a pack. It rejects entries that escape the destination
// (zip-slip) and refuses symlinks.
func unzip(src, dest string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()

	for _, f := range zr.File {
		if f.FileInfo().Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("llama: pack contains a symlink (%s); refusing", f.Name)
		}
		target := filepath.Join(dest, filepath.Clean("/"+f.Name))
		rel, err := filepath.Rel(dest, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("llama: pack entry %q escapes the destination directory", f.Name)
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
		if err := writeEntry(f, target); err != nil {
			return err
		}
	}
	return nil
}

func writeEntry(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	mode := f.FileInfo().Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	// Binaries need the exec bit on unix; the pack's own mode bits carry it.
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	// Bounded copy: a pack entry claiming a huge size should not fill the disk.
	const maxEntry = 2 << 30 // 2 GiB
	if _, err := io.Copy(out, io.LimitReader(rc, maxEntry)); err != nil {
		return err
	}
	return nil
}
