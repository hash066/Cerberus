//go:build darwin

package gpu

// sources on macOS: NONE. macOS nodes report VRAM unknown, on purpose.
//
// This is the one platform the GPU lane could not honestly probe, so it says so
// rather than inventing a number. The reasoning, because "we didn't get to it" and
// "it cannot be done from here" are very different claims and this is the latter:
//
//  1. On Apple Silicon there is no separate VRAM pool at all. GPU and CPU share one
//     physical memory; "VRAMFree" is close to a category error. The meaningful
//     quantity is Metal's per-device recommendedMaxWorkingSetSize — the working set
//     the GPU is advised to stay within, which on Apple Silicon defaults to a large
//     fraction of system RAM and can be overridden via the iogpu.wired_limit_mb
//     sysctl.
//
//  2. recommendedMaxWorkingSetSize lives on MTLDevice — an Objective-C API. Reading
//     it requires cgo. The shipped binaries are built CGO_ENABLED=0 across the whole
//     release matrix (build/release.sh), so the Metal probe is unreachable from the
//     binary users actually run. A cgo-only-on-darwin build would also forfeit
//     darwin cross-compilation from the Linux/Windows CI hosts.
//
//  3. The tempting non-cgo shortcut — `sysctl hw.memsize` times Apple's documented
//     ~75% default fraction — is REFUSED here. It would produce a confident,
//     plausible, unmeasured number, indistinguishable downstream from a real probe,
//     and it is wrong exactly when it matters: under memory pressure, or on a
//     machine whose wired limit was tuned. Lane L would then size a llama.cpp
//     --tensor-split against a guess. Unknown is strictly more useful than that: it
//     makes the node infeasible for VRAM-gated placement, which is a safe default a
//     human can knowingly override.
//
// The two honest ways to close this gap, either of which slots into the slice below
// without touching the probe core:
//
//   - Append a source backed by Lane L's llama.cpp pack device listing. llama.cpp's
//     Metal backend reports the real recommendedMaxWorkingSetSize through ggml, and
//     Lane L already runs it as a no-cgo subprocess. That measures the right
//     quantity on the right API, and costs this package no cgo — at the price of
//     only working once a pack is installed.
//   - Accept cgo on darwin specifically and call MTLDevice directly (see item 2's
//     cost).
//
// Until then macOS nodes are honestly unmeasured, and docs/vram.md says so in the
// user-facing status table.
func sources() []source { return nil }
