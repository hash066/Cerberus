//go:build !windows && !linux

package inference

import (
	"context"
	"fmt"
)

const llamacppSupportedPlatform = false

func llamacppSupported() bool {
	return false
}

// Complete is unavailable on unsupported platforms.
func Complete(ctx context.Context, prompt string, maxTokens int) (string, string, error) {
	return "", "", fmt.Errorf("llamacpp: unsupported platform (build for windows or linux)")
}

// CLIPathForTest is unavailable on unsupported platforms.
func CLIPathForTest() (string, error) {
	return "", fmt.Errorf("llamacpp: unsupported platform")
}

// ModelPathForTest is unavailable on unsupported platforms.
func ModelPathForTest() (string, error) {
	return "", fmt.Errorf("llamacpp: unsupported platform")
}
