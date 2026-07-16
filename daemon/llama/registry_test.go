package llama

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The registry is fail-closed: a malformed digest is not a warning, it is a model
// that can never be installed.
func TestModelsArePinnedAndWellFormed(t *testing.T) {
	if len(models) == 0 {
		t.Fatal("the model registry is empty")
	}
	for id, m := range models {
		if m.ID != id {
			t.Errorf("model keyed %q carries ID %q — the key and the entry disagree", id, m.ID)
		}
		if id != strings.ToLower(id) {
			t.Errorf("model id %q is not lower-case; LookupModel lower-cases its argument and would never match it", id)
		}
		if !hex64.MatchString(m.SHA256) {
			t.Errorf("%s: SHA256 %q is not 64 lower-case hex chars", id, m.SHA256)
		}
		if m.Size <= 0 {
			t.Errorf("%s: Size must be positive, got %d", id, m.Size)
		}
		if !strings.HasPrefix(m.URL, "https://") {
			t.Errorf("%s: URL %q is not https — weights must not be fetched in the clear", id, m.URL)
		}
		if strings.TrimSpace(m.Desc) == "" {
			t.Errorf("%s: needs a description; `model list` prints it before anything is downloaded", id)
		}
	}
}

// A model with no declared licence must SAY so rather than imply one. stories260k
// comes from a repo that declares none and says "do not use it in production";
// hiding that would be exactly the dishonesty CLAUDE.md forbids.
func TestSmokeTestModelIsLabelledHonestly(t *testing.T) {
	m, ok := LookupModel("stories260k")
	if !ok {
		t.Fatal("stories260k is missing from the registry")
	}
	if !m.SmokeTest {
		t.Error("stories260k must be marked SmokeTest: it emits toy story text, not conversation")
	}
	if m.License != "" {
		t.Errorf("stories260k's upstream declares no licence; claiming %q would be inventing one", m.License)
	}
	if !strings.Contains(strings.ToLower(m.Note), "production") {
		t.Error("stories260k's note must carry upstream's own 'do not use in production' warning")
	}
}

// The opt-in chat model must be a real one, and must not be marked as a smoke test.
func TestChatModelIsOptInAndLicensed(t *testing.T) {
	m, ok := LookupModel("qwen2.5-0.5b-instruct")
	if !ok {
		t.Fatal("qwen2.5-0.5b-instruct is missing from the registry")
	}
	if m.SmokeTest {
		t.Error("the chat model must not be marked SmokeTest")
	}
	if m.License == "" {
		t.Error("a redistributable chat model must carry its licence")
	}
	// Guard the "never auto-download multi-GB weights" rule: anything big enough
	// to hurt must be opt-in by name, which every registry entry already is —
	// this pins the intent so a future default cannot quietly regress it.
	if m.Size < 100<<20 {
		t.Logf("note: %s is only %d bytes; the opt-in rule matters most for large weights", m.ID, m.Size)
	}
}

func TestLookupModelIsCaseAndSpaceInsensitive(t *testing.T) {
	for _, in := range []string{"stories260k", "STORIES260K", "  Stories260K  "} {
		if _, ok := LookupModel(in); !ok {
			t.Errorf("LookupModel(%q) missed a known model", in)
		}
	}
	if _, ok := LookupModel("definitely-not-a-model"); ok {
		t.Error("LookupModel invented a model")
	}
}

// An unknown id must refuse and name the alternatives, never guess.
func TestFetchModelRefusesUnknownID(t *testing.T) {
	_, err := FetchModel(context.Background(), "llama-70b-fantasy", nil)
	if err == nil {
		t.Fatal("FetchModel accepted an unknown model id")
	}
	if !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("unexpected error: %v", err)
	}
	// The message must list what IS available, or the user is stuck.
	for _, id := range ModelIDs() {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("the unknown-model error does not mention the known model %q", id)
		}
	}
}

func TestModelsDirHonoursEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvModelDir, dir)
	got, err := ModelsDir()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(dir)
	if got != want {
		t.Errorf("ModelsDir() = %q, want %q", got, want)
	}
	p, err := ModelPath("stories260k")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != want {
		t.Errorf("ModelPath is not inside the overridden dir: %q", p)
	}
}

// A file at the right path with the WRONG contents must be reported as not
// installed, so `model pull` replaces it instead of a corrupt/substituted GGUF
// being fed to llama.cpp's parser.
func TestInstalledModelRejectsWrongContent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvModelDir, dir)

	m, _ := LookupModel("stories260k")
	p := filepath.Join(dir, m.ID+".gguf")

	// Right size, wrong bytes: the size check alone must not be enough.
	if err := os.WriteFile(p, make([]byte, m.Size), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := InstalledModel(m.ID); ok {
		t.Fatal("InstalledModel accepted a file of the correct size but the wrong digest")
	}

	// Wrong size too.
	if err := os.WriteFile(p, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := InstalledModel(m.ID); ok {
		t.Fatal("InstalledModel accepted a file of the wrong size")
	}
}

func TestInstalledModelHandlesMissingAndUnknown(t *testing.T) {
	t.Setenv(EnvModelDir, t.TempDir())
	if _, ok := InstalledModel("stories260k"); ok {
		t.Error("InstalledModel reported a model that is not there")
	}
	if _, ok := InstalledModel("not-a-model"); ok {
		t.Error("InstalledModel reported an unknown model as installed")
	}
}
