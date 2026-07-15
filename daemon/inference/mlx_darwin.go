//go:build darwin

package inference

// mlxSupportedPlatform: the MLX sidecar can only genuinely run on macOS —
// the mlx pip package requires Apple Silicon. Non-darwin builds compile the
// same client code but report unsupported (see mlx_other.go).
const mlxSupportedPlatform = true
