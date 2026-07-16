package main

// `cerberus model` and `cerberus llama` — the two things an operator has to fetch
// before real inference can run, kept explicit and separate.
//
// NOTHING HERE IS AUTOMATIC. The daemon never downloads a model or a llama.cpp
// pack on its own: weights are hundreds of megabytes and the connection is the
// user's, so both are typed commands with a visible progress bar. `model list`
// prints sizes BEFORE anything is fetched, so "469MB" is never a surprise.
//
// Both fetchers are fail-closed against a sha256 compiled into this binary. See
// daemon/llama/pack.go and daemon/llama/registry.go for why that matters: a llama
// pack is executable code, so an unverified download is arbitrary code execution.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"github.com/hash066/cerberus/daemon/llama"
)

// progressBar renders download progress to stderr, so --json stdout stays clean
// and pipeable. It no-ops when stderr is not a terminal-ish stream... it prints
// on a single carriage-returned line either way, which is fine for a log.
func progressBar(label string) llama.Progress {
	last := time.Now().Add(-time.Second)
	return func(done, total int64) {
		// Throttle: this is called once per 128KB chunk.
		if time.Since(last) < 100*time.Millisecond && done != total {
			return
		}
		last = time.Now()
		if total > 0 {
			pct := float64(done) / float64(total) * 100
			fmt.Fprintf(os.Stderr, "\r  %s  %6.1f%%  %s / %s      ",
				label, pct, humanBytes(uint64(done)), humanBytes(uint64(total)))
		} else {
			fmt.Fprintf(os.Stderr, "\r  %s  %s      ", label, humanBytes(uint64(done)))
		}
	}
}

func endProgress() { fmt.Fprintln(os.Stderr) }

// signalCtx cancels on Ctrl-C so a 469MB download is interruptible and leaves no
// partial file at the final path (the fetchers download to a temp file and rename
// only after the digest verifies).
func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

// ---- cerberus model ---------------------------------------------------------

type modelJSON struct {
	ID        string `json:"id"`
	Desc      string `json:"description"`
	Size      int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	License   string `json:"license,omitempty"`
	URL       string `json:"url"`
	Note      string `json:"note,omitempty"`
	SmokeTest bool   `json:"smoke_test"`
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
}

func modelToJSON(m llama.Model) modelJSON {
	p, installed := llama.InstalledModel(m.ID)
	return modelJSON{
		ID: m.ID, Desc: m.Desc, Size: m.Size, SHA256: m.SHA256,
		License: m.License, URL: m.URL, Note: m.Note, SmokeTest: m.SmokeTest,
		Installed: installed, Path: p,
	}
}

func cmdModel(args []string, jsonOut bool) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "model: need a subcommand: list | pull | path")
		fmt.Fprintln(os.Stderr, "usage: cerberus model list")
		fmt.Fprintln(os.Stderr, "       cerberus model pull <id>")
		fmt.Fprintln(os.Stderr, "       cerberus model path <id>")
		return exitUsage
	}
	sub, rest := args[0], args[1:]

	switch sub {
	case "list":
		return cmdModelList(jsonOut)
	case "pull":
		return cmdModelPull(rest, jsonOut)
	case "path":
		return cmdModelPath(rest, jsonOut)
	default:
		fmt.Fprintf(os.Stderr, "model: unknown subcommand %q (list | pull | path)\n", sub)
		return exitUsage
	}
}

func cmdModelList(jsonOut bool) int {
	all := llama.Models()
	if jsonOut {
		out := make([]modelJSON, 0, len(all))
		for _, m := range all {
			out = append(out, modelToJSON(m))
		}
		return printJSON(out)
	}

	dir, err := llama.ModelsDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "model list: %v\n", err)
		return exitErr
	}
	fmt.Printf("Models available to pull (stored in %s):\n\n", dir)
	for _, m := range all {
		status := "not installed"
		if _, ok := llama.InstalledModel(m.ID); ok {
			status = "INSTALLED"
		}
		fmt.Printf("  %-24s %9s  %s\n", m.ID, humanBytes(uint64(m.Size)), status)
		fmt.Printf("  %-24s %9s  %s\n", "", "", m.Desc)
		lic := m.License
		if lic == "" {
			lic = "none declared upstream"
		}
		fmt.Printf("  %-24s %9s  licence: %s\n", "", "", lic)
		if m.Note != "" {
			fmt.Printf("  %-24s %9s  note: %s\n", "", "", wrapNote(m.Note, 68, 39))
		}
		fmt.Println()
	}
	fmt.Println("Nothing is downloaded until you ask: cerberus model pull <id>")
	return exitOK
}

// wrapNote soft-wraps a note to width, indenting continuation lines by indent.
func wrapNote(s string, width, indent int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := 0
	for i, w := range words {
		if line > 0 && line+1+len(w) > width {
			b.WriteString("\n" + strings.Repeat(" ", indent))
			line = 0
		} else if i > 0 && line > 0 {
			b.WriteString(" ")
			line++
		}
		b.WriteString(w)
		line += len(w)
	}
	return b.String()
}

func cmdModelPull(args []string, jsonOut bool) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "model pull: need a model id")
		fmt.Fprintf(os.Stderr, "usage: cerberus model pull <id>   (known: %s)\n", strings.Join(llama.ModelIDs(), ", "))
		return exitUsage
	}
	id := args[0]

	m, ok := llama.LookupModel(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "model pull: unknown model %q\n", id)
		fmt.Fprintf(os.Stderr, "known models: %s\n", strings.Join(llama.ModelIDs(), ", "))
		fmt.Fprintln(os.Stderr, "(cerberus model list shows sizes and licences)")
		return exitUsage
	}

	if p, ok := llama.InstalledModel(m.ID); ok {
		if jsonOut {
			return printJSON(modelToJSON(m))
		}
		fmt.Printf("%s is already installed and matches its pinned digest.\n  %s\n", m.ID, p)
		return exitOK
	}

	// Say what is about to happen, in bytes, before it happens.
	if !jsonOut {
		fmt.Printf("Pulling %s (%s)\n", m.ID, humanBytes(uint64(m.Size)))
		fmt.Printf("  from %s\n", m.URL)
		fmt.Printf("  sha256 %s\n", m.SHA256)
		if m.License == "" {
			fmt.Println("  licence: NONE DECLARED upstream — check the source before relying on this.")
		} else {
			fmt.Printf("  licence: %s\n", m.License)
		}
		if m.SmokeTest {
			fmt.Println("  NOTE: this is a smoke-test fixture, not a usable assistant.")
		}
		fmt.Println()
	}

	ctx, cancel := signalCtx()
	defer cancel()

	var prog llama.Progress
	if !jsonOut {
		prog = progressBar("downloading")
	}
	path, err := llama.FetchModel(ctx, m.ID, prog)
	if prog != nil {
		endProgress()
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "model pull: cancelled; nothing was installed.")
			return exitErr
		}
		fmt.Fprintf(os.Stderr, "model pull: %v\n", err)
		return exitErr
	}

	if jsonOut {
		return printJSON(modelToJSON(m))
	}
	fmt.Printf("Installed %s\n  %s\n", m.ID, path)
	fmt.Println("  digest verified against the value compiled into this binary.")
	return exitOK
}

func cmdModelPath(args []string, jsonOut bool) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "model path: need a model id")
		return exitUsage
	}
	id := args[0]
	m, ok := llama.LookupModel(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "model path: unknown model %q (known: %s)\n", id, strings.Join(llama.ModelIDs(), ", "))
		return exitUsage
	}
	p, installed := llama.InstalledModel(m.ID)
	if !installed {
		want, err := llama.ModelPath(m.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "model path: %v\n", err)
			return exitErr
		}
		fmt.Fprintf(os.Stderr, "model path: %s is not installed (would live at %s)\n", m.ID, want)
		fmt.Fprintf(os.Stderr, "pull it with: cerberus model pull %s\n", m.ID)
		return exitErr
	}
	if jsonOut {
		return printJSON(modelToJSON(m))
	}
	// Bare path on stdout so it composes: -m "$(cerberus model path stories260k)"
	fmt.Println(p)
	return exitOK
}

// ---- cerberus llama ---------------------------------------------------------

type llamaStatusJSON struct {
	PinnedTag string `json:"pinned_tag"`
	MinBuild  int    `json:"min_build"`
	Platform  string `json:"platform"`
	Backend   string `json:"backend"`
	Asset     string `json:"asset,omitempty"`
	AssetSize int64  `json:"asset_size_bytes,omitempty"`
	Installed bool   `json:"installed"`
	PackDir   string `json:"pack_dir,omitempty"`
	Server    string `json:"llama_server,omitempty"`
	RPCServer string `json:"ggml_rpc_server,omitempty"`
	Build     int    `json:"build,omitempty"`
	Commit    string `json:"commit,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func cmdLlama(args []string, jsonOut bool) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "llama: need a subcommand: status | fetch")
		fmt.Fprintln(os.Stderr, "usage: cerberus llama status")
		fmt.Fprintln(os.Stderr, "       cerberus llama fetch [--backend vulkan|metal|cpu]")
		return exitUsage
	}
	switch args[0] {
	case "status":
		return cmdLlamaStatus(jsonOut)
	case "fetch":
		return cmdLlamaFetch(args[1:], jsonOut)
	default:
		fmt.Fprintf(os.Stderr, "llama: unknown subcommand %q (status | fetch)\n", args[0])
		return exitUsage
	}
}

func cmdLlamaStatus(jsonOut bool) int {
	backend := llama.DefaultBackend()
	plat := runtime.GOOS + "/" + runtime.GOARCH
	st := llamaStatusJSON{
		PinnedTag: llama.PinnedTag,
		MinBuild:  llama.MinBuild,
		Platform:  plat,
		Backend:   string(backend),
	}
	if name := llama.PackName(runtime.GOOS, runtime.GOARCH, backend); name != "" {
		st.Asset = name
		if a, ok := llama.PackAssetFor(runtime.GOOS, runtime.GOARCH, backend); ok {
			st.AssetSize = a.Size
		}
	}
	if d, err := llama.PackDir(); err == nil {
		st.PackDir = d
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bins, err := llama.Locate(ctx)
	if err == nil {
		st.Installed = true
		st.Server, st.RPCServer, st.Build, st.Commit = bins.Server, bins.RPCServer, bins.Build, bins.Commit
	} else {
		st.Reason = err.Error()
	}

	if jsonOut {
		return printJSON(st)
	}

	fmt.Printf("llama.cpp pack\n")
	fmt.Printf("  pinned tag  : %s (refuses anything older than b%d — CVE-2026-34159)\n", st.PinnedTag, st.MinBuild)
	fmt.Printf("  platform    : %s\n", st.Platform)
	fmt.Printf("  backend     : %s\n", st.Backend)
	if st.Asset != "" {
		fmt.Printf("  asset       : %s (%s)\n", st.Asset, humanBytes(uint64(st.AssetSize)))
	} else {
		fmt.Printf("  asset       : none pinned for this platform\n")
	}
	fmt.Printf("  pack dir    : %s\n", st.PackDir)
	fmt.Println()
	if st.Installed {
		fmt.Printf("  INSTALLED, and it passed the version gate:\n")
		fmt.Printf("    build         : b%d (%s)\n", st.Build, st.Commit)
		fmt.Printf("    llama-server  : %s\n", st.Server)
		fmt.Printf("    ggml-rpc-server: %s\n", st.RPCServer)
		return exitOK
	}
	fmt.Printf("  NOT USABLE:\n    %s\n", strings.ReplaceAll(st.Reason, "\n", "\n    "))
	fmt.Println()
	fmt.Println("  fetch it with: cerberus llama fetch")
	// Not an error: "no pack installed" is a normal state for a node that never
	// runs a model. It is reported, not treated as a failure.
	return exitOK
}

func cmdLlamaFetch(args []string, jsonOut bool) int {
	backend := llama.DefaultBackend()
	if v, rest := extractValueFlag(args, "--backend"); v != "" {
		args = rest
		switch strings.ToLower(v) {
		case "vulkan":
			backend = llama.BackendVulkan
		case "metal":
			backend = llama.BackendMetal
		case "cpu":
			backend = llama.BackendCPU
		default:
			fmt.Fprintf(os.Stderr, "llama fetch: unknown backend %q (vulkan | metal | cpu)\n", v)
			return exitUsage
		}
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "llama fetch: unexpected argument %q\n", args[0])
		return exitUsage
	}

	asset, ok := llama.PackAssetFor(runtime.GOOS, runtime.GOARCH, backend)
	if !ok {
		fmt.Fprintf(os.Stderr,
			"llama fetch: no pinned pack for %s/%s with backend %q.\n"+
				"Cerberus only fetches llama.cpp binaries whose sha256 is compiled in — it will not\n"+
				"download unverified executable code. Upstream may publish nothing for this\n"+
				"combination; %s/%s defaults to backend %q.\n",
			runtime.GOOS, runtime.GOARCH, backend, runtime.GOOS, runtime.GOARCH, llama.DefaultBackend())
		return exitErr
	}

	if !jsonOut {
		fmt.Printf("Fetching llama.cpp %s pack for %s/%s (%s)\n", llama.PinnedTag, runtime.GOOS, runtime.GOARCH, backend)
		fmt.Printf("  asset  %s (%s)\n", asset.Name, humanBytes(uint64(asset.Size)))
		fmt.Printf("  sha256 %s\n", asset.SHA256)
		fmt.Println("  source: upstream ggml-org/llama.cpp releases (MIT)")
		fmt.Println()
	}

	ctx, cancel := signalCtx()
	defer cancel()

	var prog llama.Progress
	if !jsonOut {
		prog = progressBar("downloading")
	}
	bins, err := llama.FetchPack(ctx, backend, prog)
	if prog != nil {
		endProgress()
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "llama fetch: cancelled; nothing was installed.")
			return exitErr
		}
		fmt.Fprintf(os.Stderr, "llama fetch: %v\n", err)
		return exitErr
	}

	if jsonOut {
		return printJSON(llamaStatusJSON{
			PinnedTag: llama.PinnedTag, MinBuild: llama.MinBuild,
			Platform: runtime.GOOS + "/" + runtime.GOARCH, Backend: string(backend),
			Asset: asset.Name, AssetSize: asset.Size, Installed: true,
			PackDir: bins.Dir, Server: bins.Server, RPCServer: bins.RPCServer,
			Build: bins.Build, Commit: bins.Commit,
		})
	}
	fmt.Printf("Installed llama.cpp b%d (%s)\n", bins.Build, bins.Commit)
	fmt.Printf("  %s\n", bins.Dir)
	fmt.Println("  digest verified, and the binaries passed the CVE-2026-34159 version gate.")
	return exitOK
}
