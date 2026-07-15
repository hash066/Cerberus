//go:build !windows && !linux && !darwin

package gpu

// sources on every other GOOS (freebsd, openbsd, …): nvidia-smi if the driver
// happens to ship it, otherwise unknown. These platforms are outside the release
// matrix (build/release.sh ships {linux,darwin,windows} x {amd64,arm64}) and are
// untested by this lane; the probe is here so the package builds and degrades to an
// honest "unknown" rather than failing to compile.
func sources() []source {
	return []source{
		{name: nvidiaSMISourceName, probe: probeNvidiaSMI},
	}
}
