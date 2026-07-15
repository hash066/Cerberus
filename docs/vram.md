# VRAM telemetry — what the node reports, and how it knows

The scheduler ranks nodes for GPU work on `Memory.VRAMFree`
([`daemon/scheduler.DefaultCostModel`](../daemon/scheduler/scheduler.go): it drops
a node whose `VRAMFree < MinVRAM` and scores the rest by free VRAM). Lane L sizes a
llama.cpp `--tensor-split` from the same number. So a wrong VRAM figure does not
degrade gracefully — it sends a model to a card that cannot hold it.

[`daemon/gpu.VRAMSnapshot()`](../daemon/gpu/vramprobe.go) is the measurement. It
obeys one rule, the same one `cerberus gpu`'s `backend:` line obeys
([docs/gpu.md](gpu.md)):

> **Never fabricate.** Every number names the source that measured it, and a
> machine we cannot measure reports **unknown**, not a plausible default.

## Status by platform

| platform | source | status |
|---|---|---|
| Windows + NVIDIA | `nvidia-smi` | **verified** on an RTX 3050 Laptop GPU — exact match vs `nvidia-smi` |
| Linux + NVIDIA | `nvidia-smi` | **verified** (WSL2 Debian, kernel 6.6, NVIDIA passthrough) — exact match |
| Linux + AMD | `amdgpu-sysfs` | parser unit-tested against a synthetic sysfs tree; **unverified on real AMD hardware** |
| Windows + Intel/AMD | — | **unknown** (see "Why not DXGI") |
| macOS (Apple Silicon) | — | **unknown** (see "Why macOS reports unknown") |

"Unknown" reports `VRAMTotal = VRAMFree = 0`, which makes the node infeasible for
VRAM-gated placement. That is the intended, safe outcome: a node whose VRAM we
cannot see does not get GPU work by default.

## The proof

Measured on the lane's test box (Windows 11, RTX 3050 Laptop GPU + Intel Iris Xe),
probe vs. `nvidia-smi` at the same moment:

```
probe:      vram: NVIDIA GeForce RTX 3050 Laptop GPU 3861 MiB free / 4096 MiB total (source: nvidia-smi)
nvidia-smi: 0, NVIDIA GeForce RTX 3050 Laptop GPU, 4096 MiB, 104 MiB, 3861 MiB
```

Note what the old hardcoded literals claimed: `VRAMTotal: 8_000_000_000,
VRAMFree: 6_000_000_000` — **2x this card's actual size**. Sizing a tensor-split
against "6 GB free" on a card with 3861 MiB free is an OOM.

Note also `4096 - 104 = 3992 ≠ 3861`. The driver reserves ~131 MiB that appears in
neither `total` nor `used`. This is why the probe queries `memory.free` directly
instead of computing `total - used`: the subtraction would overstate free by 131 MiB.

## Why a subprocess, and not NVML / DXGI / Vulkan

The releases ship `CGO_ENABLED=0` across `{linux,darwin,windows} x {amd64,arm64}`
([build/release.sh](../build/release.sh)). **Any probe must therefore be pure Go**,
which rules out NVML and Metal (C APIs) outright. What remains is OS interfaces via
`syscall`, sysfs, and subprocesses.

`nvidia-smi` wins on cost and honesty: it ships inside every NVIDIA driver on both
Windows (`C:\Windows\System32\nvidia-smi.exe`) and Linux (`/usr/bin/nvidia-smi`),
needs no SDK or headers, and is *the same tool a human runs to check VRAM* — so the
scheduler and the operator can never silently disagree.

### Why not DXGI

`IDXGIAdapter3::QueryVideoMemoryInfo` is the obvious Windows answer — in-process,
vendor-neutral, reachable from pure Go. It was prototyped and measured against
`nvidia-smi` on the test box, and **rejected on the data**:

| adapter | DXGI `DedicatedVideoMemory` | DXGI LOCAL `Budget` | `nvidia-smi` |
|---|---|---|---|
| NVIDIA RTX 3050 | 3964 MiB | 3369 MiB | 4096 total / 3861 free |
| Intel Iris Xe | 128 MiB | **7345 MiB** | (invisible) |

DXGI measures a *different quantity*. `Budget` is what the OS suggests **this
process** stay under, so it is not the device's free memory — 3369 vs 3861 MiB on
the same card at the same moment. Worse, on the integrated GPU it reports **7345 MiB
of shared system RAM** for a part with 128 MiB dedicated: exactly the
plausible-looking number that would OOM a node. Reporting `unknown` for an iGPU box
is both honest and the correct scheduling outcome.

(A related trap, found while measuring: `DXGI_ADAPTER_DESC1`'s memory fields are
`SIZE_T`, so a 32-bit build reads a shifted struct and reported **3072 MiB** for the
same 4 GiB card. The release matrix is 64-bit, but the dev box's default Go
toolchain is `windows/386` — a DXGI probe would have been wrong in development and
right in production.)

If vendor-neutral Windows VRAM becomes a real requirement, the honest routes are
Vulkan's `VK_EXT_memory_budget` (true device-local heap budget/usage, dlopen-able
from pure Go) or a llama.cpp device listing. Both measure the right quantity, and
either drops into the `sources()` slice without touching the probe core.

### Why macOS reports unknown

On Apple Silicon there is no separate VRAM pool — GPU and CPU share one physical
memory, and "VRAMFree" is close to a category error. The meaningful quantity is
Metal's `recommendedMaxWorkingSetSize`, which lives on `MTLDevice`: an Objective-C
API needing cgo, which `CGO_ENABLED=0` releases cannot reach.

The tempting shortcut — `sysctl hw.memsize` × Apple's documented ~75% default — is
**refused**. It would produce a confident, unmeasured number indistinguishable
downstream from a real probe, and it is wrong exactly when it matters (under memory
pressure, or on a machine with a tuned `iogpu.wired_limit_mb`).

The clean fix is to append a source backed by Lane L's llama.cpp pack: its Metal
backend reports the real `recommendedMaxWorkingSetSize` through ggml, and Lane L
already runs it as a no-cgo subprocess. That measures the right quantity on the
right API — at the price of only working once a pack is installed.

## Cost and caching

`nvidia-smi` costs ~100–130 ms per call. Telemetry samples at **2 Hz**
([`daemon/system`](../daemon/system/system.go) composes `telemetry.Config{Hz: 2}`),
so an uncached probe would burn ~26% of a core. `gpu.Monitor` caches behind a 5 s
TTL and serves stale while revalidating: only a cold `Get` blocks, and a wedged
`nvidia-smi` can never stall the telemetry tick. Every snapshot carries `AsOf`, so
staleness is always visible.

## Reading it

```go
v := gpu.VRAMSnapshot()
if !v.Known() {
    // honest absence — do not substitute a default
}
v.Total() // best single device's total, bytes
v.Free()  // best single device's free, bytes
v.Devices // per-device, for consumers that split across GPUs (--tensor-split)
```

`Total()`/`Free()` reduce to the **best single device, never a sum**: a task runs on
one GPU and can only use that GPU's memory. Reporting a laptop's 4 GiB dGPU plus a
128 MiB iGPU as "4.1 GiB free" would be the same class of lie this package exists to
prevent.

## Known gaps

- **AMD on Linux is unverified on hardware.** The parser is tested; the real-driver
  path is not. It fails closed.
- **Intel/AMD on Windows report unknown**, by the DXGI reasoning above.
- **macOS reports unknown**, by the Metal/cgo reasoning above.
- **The VRAM device's grantable quota is still a hardcoded 2 GiB literal**
  (`daemon/system/system.go`'s `contract.Quota{Bytes: 2 * 1024 * 1024 * 1024}`, and
  the same literal duplicated in `cmd/cerberusd/main.go`'s devices catalogue). It is
  unrelated to the card actually present, and now that the probe exists it should be
  derived from it. Telemetry is real; **the device quota is not yet**.
- **Live VRAM accounting is not wired.** The composed daemon does not debit a node's
  VRAM as sessions run, so `VRAMFree` reflects what the driver reports right now
  (which does include other processes' usage), not Cerberus's own outstanding grants.
