package llama

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The model registry: the small, curated set of GGUF weights `cerberus model pull`
// knows how to fetch, each pinned to a sha256 this binary was compiled with.
//
// # Nothing here is ever downloaded automatically
//
// Weights are big and the network is the user's. NOTHING in this file runs unless
// an operator types `cerberus model pull <id>`. The daemon never reaches for a
// model on its own, and there is no "download on first chat" path. A multi-GB
// surprise on someone's tether is not a feature.
//
// # Why a registry at all, rather than "pass a URL"
//
// A GGUF is not executable, but it is untrusted input to a C++ parser that runs in
// this process tree, and llama.cpp's GGUF reader has had its share of CVEs. Pinning
// a digest means `model pull` either produces the exact bytes we vetted or fails.
// Operators who want something else can still point -m at any file they like; that
// is their call to make explicitly, not ours to make for them.

// Model is one entry in the built-in registry.
type Model struct {
	// ID is the name `cerberus model pull <id>` takes.
	ID string
	// Desc is a one-line human description.
	Desc string
	// URL is where the GGUF is fetched from. Redirects are followed.
	URL string
	// SHA256 is the digest of the file at URL, verified before it is installed.
	SHA256 string
	// Size is the file's length in bytes.
	Size int64
	// License is the upstream licence, or "" when upstream declares none.
	License string
	// Note is the honesty note shown by `cerberus model list`.
	Note string
	// SmokeTest marks a model that exists to prove the plumbing works and is NOT
	// a useful assistant.
	SmokeTest bool
}

// models is the registry, keyed by ID.
//
// Every digest below was verified on 2026-07-16 by downloading the file and
// hashing it locally with sha256sum, then cross-checking against the publisher's
// own metadata (HuggingFace's X-Linked-ETag / LFS oid, which is the file's
// sha256). Do not add an entry whose digest you have not verified this way: this
// map is fail-closed, so a wrong digest is not a warning, it is a model that can
// never be installed.
var models = map[string]Model{
	// ~1.1MB. Downloading this is not a meaningful cost, which is why it is the
	// default smoke test.
	"stories260k": {
		ID:     "stories260k",
		Desc:   "260K-param toy story model (smoke test only)",
		URL:    "https://huggingface.co/ggml-org/models/resolve/main/tinyllamas/stories260K.gguf",
		SHA256: "270cba1bd5109f42d03350f60406024560464db173c0e387d91f0426d3bd256d",
		Size:   1185376,
		// Upstream (ggml-org/models) declares NO licence and says, verbatim,
		// "Various models to be used in llama.cpp CI workflow. Do not use it in
		// production." Reproduced rather than papered over.
		License: "",
		Note: "1.1MB fixture from llama.cpp's CI. It emits toy story text, not " +
			"conversation, and upstream declares no licence and says not to use it in " +
			"production. Use it to prove inference runs end to end — nothing else.",
		SmokeTest: true,
	},

	// ~469MB. A real instruction-tuned chat model, and the smallest one worth
	// talking to. Opt-in: you have to ask for it by name.
	"qwen2.5-0.5b-instruct": {
		ID:      "qwen2.5-0.5b-instruct",
		Desc:    "Qwen2.5 0.5B Instruct, Q4_K_M (real chat, ~469MB)",
		URL:     "https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/main/qwen2.5-0.5b-instruct-q4_k_m.gguf",
		SHA256:  "74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db",
		Size:    491400032,
		License: "apache-2.0",
		Note: "A genuinely small chat model: it answers, but it is 0.5B parameters — " +
			"expect weak reasoning and confident errors. Fine for proving the gateway " +
			"and the mesh; not a Claude replacement.",
	},
}

// EnvModelDir overrides where models are stored.
const EnvModelDir = "CERBERUS_LLAMA_MODEL_DIR"

// ModelsDir is where pulled models live: <user cache>/cerberus/models/.
//
// NOT keyed by PinnedTag, unlike PackDir: GGUF is a stable format and a model
// outlives a llama.cpp bump. Re-downloading 469MB because the binaries moved
// forward would be user-hostile.
func ModelsDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv(EnvModelDir)); d != "" {
		return filepath.Abs(d)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "cerberus", "models"), nil
}

// Models returns the registry, sorted by ID.
func Models() []Model {
	out := make([]Model, 0, len(models))
	for _, m := range models {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// LookupModel returns the registry entry for id.
func LookupModel(id string) (Model, bool) {
	m, ok := models[strings.ToLower(strings.TrimSpace(id))]
	return m, ok
}

// ModelIDs returns the known ids, sorted — for error messages.
func ModelIDs() []string {
	out := make([]string, 0, len(models))
	for id := range models {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ModelPath returns where a registry model is (or would be) installed.
func ModelPath(id string) (string, error) {
	m, ok := LookupModel(id)
	if !ok {
		return "", unknownModel(id)
	}
	dir, err := ModelsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, m.ID+".gguf"), nil
}

func unknownModel(id string) error {
	return fmt.Errorf("llama: unknown model %q. Known models: %s.\n"+
		"The registry is a fixed set of digest-pinned GGUFs (see daemon/llama/registry.go). "+
		"To run weights that are not in it, point the daemon at the file directly rather than "+
		"pulling it through this command.",
		id, strings.Join(ModelIDs(), ", "))
}

// InstalledModel reports whether a registry model is present locally AND matches
// its pinned digest. A file of the right size but the wrong hash is reported as
// not installed, so `model pull` replaces it.
func InstalledModel(id string) (string, bool) {
	p, err := ModelPath(id)
	if err != nil {
		return "", false
	}
	m, ok := LookupModel(id)
	if !ok {
		return "", false
	}
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() || fi.Size() != m.Size {
		return "", false
	}
	sum, err := sha256File(p)
	if err != nil || sum != m.SHA256 {
		return "", false
	}
	return p, true
}

// FetchModel downloads a registry model, verifies its sha256, and installs it into
// ModelsDir. It is a no-op if the model is already installed and verifies.
//
// This is ONLY reached from `cerberus model pull`. It is never called implicitly.
func FetchModel(ctx context.Context, id string, progress Progress) (string, error) {
	m, ok := LookupModel(id)
	if !ok {
		return "", unknownModel(id)
	}
	if p, ok := InstalledModel(m.ID); ok {
		return p, nil
	}

	dir, err := ModelsDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	final := filepath.Join(dir, m.ID+".gguf")

	tmp, err := os.CreateTemp(dir, "."+m.ID+"-*.part")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	// Reuses pack.go's download: same bounded, hashing, size-checked reader.
	got, n, err := download(ctx, m.URL, tmp, m.Size, progress)
	_ = tmp.Close()
	if err != nil {
		return "", err
	}
	if got != m.SHA256 {
		return "", fmt.Errorf(
			"llama: SHA256 MISMATCH for model %s\n  want %s\n  got  %s\n"+
				"The download was discarded and nothing was installed. Either it was corrupted or "+
				"tampered with in transit, or the publisher replaced the file and the pinned digest "+
				"in daemon/llama/registry.go is stale.",
			m.ID, m.SHA256, got)
	}
	if n != m.Size {
		return "", fmt.Errorf("llama: model %s verified by digest but its length is %d, not the pinned %d", m.ID, n, m.Size)
	}

	// Rename into place only after the digest is proven: a reader must never see a
	// partial file at the final path.
	if err := os.Remove(final); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", fmt.Errorf("llama: installing model %s: %w", m.ID, err)
	}
	if err := os.Chmod(final, 0o644); err != nil {
		return "", err
	}
	return final, nil
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
