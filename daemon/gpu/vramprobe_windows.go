//go:build windows

package gpu

// sources on Windows: nvidia-smi only.
//
// # Why not DXGI (IDXGIAdapter3::QueryVideoMemoryInfo), which is the obvious choice
//
// DXGI is attractive on paper: an in-process syscall (no subprocess), pure Go via
// LoadLibrary, and vendor-neutral — it would cover Intel/AMD GPUs that nvidia-smi
// cannot see. It was prototyped and measured against nvidia-smi on this repo's test
// box (RTX 3050 Laptop + Intel Iris Xe, Windows 11). It was rejected on the data:
//
//	adapter                      DXGI DedicatedVideoMemory   DXGI LOCAL Budget    nvidia-smi
//	NVIDIA GeForce RTX 3050      3964 MiB                    3369 MiB             4096 total / 3861 free
//	Intel(R) Iris(R) Xe Graphics  128 MiB                    7345 MiB             (invisible)
//
// Three findings, each disqualifying on its own:
//
//  1. DXGI measures a DIFFERENT QUANTITY than "free VRAM". `Budget` is the amount
//     the OS suggests THIS PROCESS stay under, and `CurrentUsage` is likewise
//     per-process — so Budget-CurrentUsage is our own headroom, not the device's
//     free memory. On the 3050 it read 3369 MiB against nvidia-smi's 3861 MiB free:
//     a 492 MiB disagreement that no amount of care in the caller can reconcile,
//     because the two numbers mean different things.
//
//  2. On an integrated GPU it is actively dangerous. The Iris Xe has 128 MiB of
//     dedicated memory, but DXGI reports a 7345 MiB budget — that is SHARED SYSTEM
//     RAM, not VRAM. Feeding 7 GiB of "VRAM free" to a llama.cpp --tensor-split
//     would be the exact plausible-looking lie that OOMs a node. Reporting unknown
//     for an iGPU box, and thereby declining to place VRAM-gated work on it, is
//     both honest and the correct scheduling outcome.
//
//  3. Even DXGI's `total` disagrees: DedicatedVideoMemory read 3964 MiB where the
//     card has 4096 MiB, because it reports post-carve-out memory exposed to D3D.
//
// (A fourth, narrower trap found while measuring: DXGI_ADAPTER_DESC1's memory
// fields are SIZE_T, so a 32-bit build reads a shifted struct and produced a
// garbage 3072 MiB for the same 4 GiB card. The release matrix is 64-bit only, but
// the local Go toolchain on the test box is windows/386 — which would have made a
// DXGI probe silently wrong in development and right in production.)
//
// The consequence is deliberate and documented: on Windows, a machine with no
// NVIDIA GPU reports VRAM unknown. If vendor-neutral Windows VRAM ever becomes a
// real requirement, the honest routes are Vulkan's VK_EXT_memory_budget (which
// reports true device-local heap budget/usage, dlopen-able from pure Go) or a
// llama.cpp device listing from Lane L's pack — both measure the right quantity.
// Either can be appended to the slice below without touching the probe core.
func sources() []source {
	return []source{
		{name: nvidiaSMISourceName, probe: probeNvidiaSMI},
	}
}
