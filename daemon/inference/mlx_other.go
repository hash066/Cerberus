//go:build !darwin

package inference

// mlxSupportedPlatform: mlx is macOS/Apple-Silicon-only, so on this platform
// the sidecar is never probed and every mlx op degrades to the mock path (for
// pipeline shard forward) or a clear error (for Generate/ForwardPass).
const mlxSupportedPlatform = false
