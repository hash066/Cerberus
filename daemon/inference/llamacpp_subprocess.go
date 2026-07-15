//go:build windows || linux

package inference

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const llamacppSupportedPlatform = true

var llamacppCLINames = []string{
	"llama-cli",
	"llama-completion",
	"main", // legacy llama.cpp binary name
}

func llamacppSupported() bool {
	return llamacppSupportedPlatform
}

// Complete runs single-node text generation via llama.cpp subprocess.
// Returns generated text and the backend name that ran ("llamacpp" or error).
func Complete(ctx context.Context, prompt string, maxTokens int) (text string, backend string, err error) {
	if !llamacppSupported() {
		return "", "", fmt.Errorf("llamacpp: unsupported platform")
	}
	if mockForced() {
		return "", llamacppMockBackend, fmt.Errorf("llamacpp: mock mode enabled (CERBERUS_LLAMACPP_MOCK=1)")
	}
	model, err := resolveModelPath()
	if err != nil {
		return "", "", err
	}
	cli, err := resolveCLIPath()
	if err != nil {
		return "", "", err
	}
	if maxTokens <= 0 {
		maxTokens = 8
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
	}

	args := []string{
		"-m", model,
		"-p", prompt,
		"-n", fmt.Sprintf("%d", maxTokens),
		"--simple-io",
		"--no-display-prompt",
		"--log-disable",
		"-no-cnv",
	}
	cmd := exec.CommandContext(ctx, cli, args...)
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return "", "", fmt.Errorf("llamacpp: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", "", fmt.Errorf("llamacpp: %w", err)
	}
	text = strings.TrimSpace(string(out))
	if text == "" {
		return "", llamacppRealBackend, fmt.Errorf("llamacpp: empty output from %s", filepath.Base(cli))
	}
	return text, llamacppRealBackend, nil
}

func resolveCLIPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("CERBERUS_LLAMA_CLI")); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("llamacpp: CERBERUS_LLAMA_CLI %q: %w", p, err)
		}
		return p, nil
	}
	for _, name := range llamacppCLINames {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("llamacpp: no llama.cpp CLI found (install llama-cli or set CERBERUS_LLAMA_CLI)")
}

func resolveModelPath() (string, error) {
	for _, key := range []string{"CERBERUS_LLAMA_MODEL", "LLAMA_MODEL"} {
		if p := strings.TrimSpace(os.Getenv(key)); p != "" {
			if _, err := os.Stat(p); err != nil {
				return "", fmt.Errorf("llamacpp: %s %q: %w", key, p, err)
			}
			return p, nil
		}
	}
	return "", fmt.Errorf("llamacpp: no GGUF model path (set CERBERUS_LLAMA_MODEL or LLAMA_MODEL)")
}

// CLIPathForTest returns the resolved llama.cpp CLI path for diagnostics.
func CLIPathForTest() (string, error) {
	return resolveCLIPath()
}

// ModelPathForTest returns the resolved GGUF model path for diagnostics.
func ModelPathForTest() (string, error) {
	return resolveModelPath()
}
