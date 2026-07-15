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

// llamacppRealForwardActivation is unreachable here (llamacppSupported() is
// false, so the engine always takes the mock path) but must compile.
func llamacppRealForwardActivation(Activation, uint32, uint32) (Activation, string, error) {
	return Activation{}, "", fmt.Errorf("llamacpp: unsupported platform (build for windows or linux)")
}

func llamaModelConfigured() bool { return false }

// LlamacppCLIReady is always false on unsupported platforms.
func LlamacppCLIReady() bool { return false }
