//go:build !windows && !linux

package inference

import "fmt"

// LlamacppHelperReady is false on unsupported platforms.
func LlamacppHelperReady() bool {
	return false
}

// LlamacppReportedBackend always reports mock on unsupported platforms.
func LlamacppReportedBackend() string {
	return llamacppMockBackend
}

// HelperPathForTest is unavailable on unsupported platforms.
func HelperPathForTest() (string, error) {
	return "", fmt.Errorf("llamacpp: unsupported platform")
}
