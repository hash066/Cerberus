# GPU compute dispatch (`cerberus gpu`) — the real-GPU build

`cerberus gpu <kernel> …` runs a small element-wise f32 kernel on the daemon and
prints the result **plus the backend that actually ran it**. The `backend:` line
never lies about where the numbers came from (CLAUDE.md "maturity honesty"):

| backend        | what ran                                                        |
|----------------|-----------------------------------------------------------------|
| `cpu-software` | real CPU compute (pure Go, or the Rust software backend). The default. |
| `gpu-wgpu`     | your **physical GPU** via wgpu (Vulkan/DX12/Metal). Needs the FFI + GPU build below. |

Supported kernels (identical numerics on every backend):

```
cerberus gpu vector-add 1,2,3 4,5,6           #  a + b        -> [5 7 9]
cerberus gpu saxpy      1,2,3 0.5,0.5,0.5 --param 2   #  alpha*a + b (alpha=2)
cerberus gpu scalar-mul 1,2,3 --param 3       #  a * scalar   -> [3 6 9]
```

The **default build never touches a GPU or a C toolchain** — it computes on the
CPU and honestly reports `cpu-software`. This is what most users run. Everything
below is only needed to light up a physical GPU.

---

## How the backend is selected (honest by construction)

```
cerberus gpu ──RPC──> cerberusd ──> daemon/gpu.Dispatch
                                       │
        default build  (no -tags ffi)  │  -tags ffi  (cgo -> Rust core/cabi)
                                       ▼
                              cerberus_gpu_submit  (core/cabi)
                                       │
             cabi built WITHOUT gpu    │   cabi built --features gpu
                                       ▼
                       SoftwareGpu  ───┴───  WgpuDispatch::new()
                     "cpu-software"           │  adapter?  ──yes──> "gpu-wgpu"
                                              └────────────no───────> "cpu-software"
```

- `gpu-wgpu` requires **both** the daemon built `-tags ffi` **and** the cabi
  staticlib built `--features gpu` **and** a working GPU adapter+driver at runtime.
- If the `gpu` feature is on but no adapter is present (headless box, missing
  driver), the Rust side falls back to its software backend and reports
  `cpu-software` — it does **not** fabricate a GPU result.

`core/cabi/src/lib.rs::cerberus_gpu_backend()` is the single source of truth for
that string; the Go side (`daemon/ffi/gpu_ffi.go`) surfaces it verbatim.

---

## Get `backend: gpu-wgpu` on Windows + NVIDIA (e.g. an RTX 3050)

This is the FFI build, extended with the `gpu` feature. It reuses the exact
toolchain the capability-kernel FFI build already needs (see
[docs/ffi.md](ffi.md)); the only new ingredients are your GPU driver and the
`-Gpu` flag.

### 1. One-time toolchain (same as the FFI build)

cgo on Windows needs a **GCC-compatible C compiler** (it cannot drive MSVC
`cl.exe`), and the Rust staticlib must share that GNU ABI. We use `zig cc` (a
portable, no-admin mingw-w64-compatible C compiler/linker):

```powershell
# 1. A GNU C toolchain for cgo — download from https://ziglang.org/download
#    Unzip it, then point ZIG at zig.exe using a path WITH NO SPACES:
$env:ZIG = "C:\tools\zig\zig.exe"     # (a space in the path breaks cgo's CC parsing)

# 2. The GNU-ABI Rust std + the llvm tools the build's patch step uses:
rustup target add x86_64-pc-windows-gnu
rustup component add llvm-tools-preview
```

> If you prefer a full mingw-w64 gcc instead of zig, that also works — cgo just
> needs a gcc/clang-compatible `CC`. `zig cc` is the path the helper script is
> wired for; swap `$env:CC` if you use gcc.

Your **NVIDIA driver** already ships the Vulkan/DX12 runtime wgpu needs — no
extra install. (wgpu loads Vulkan / the D3D shader compiler at runtime; nothing
extra to link.)

### 2. Build the daemon with the real GPU backend

One command builds the `--features gpu` staticlib **and** the `-tags ffi ffigpu`
daemon:

```powershell
pwsh build/ffi.ps1 -Action build -Gpu      #  or:  task build:gpu
# -> writes cerberusd-ffi.exe
```

What `-Gpu` changes vs. the plain FFI build:
- step 1 builds cabi with `--features gpu` (so `cerberus_gpu_submit` binds the
  real wgpu device), and
- step 3 adds the `ffigpu` build tag, which links wgpu's extra Win32 system libs
  (`daemon/ffi/gpu_ffi_ld.go`).

Without `-Gpu`, `build/ffi.ps1` is unchanged: it builds the software-GPU
staticlib and the plain `-tags ffi` daemon (`cpu-software`).

### 3. Run it and confirm the GPU ran

```powershell
# start the GPU-enabled daemon you just built (not a plain `cerberusd`):
.\cerberusd-ffi.exe

# in another shell:
cerberus gpu vector-add 1,2,3 4,5,6
#   vector-add(a,b) = [5 7 9]
#   backend: gpu-wgpu        <-- your RTX GPU ran it
```

If you see `backend: cpu-software` from `cerberusd-ffi.exe`, the daemon built
fine but wgpu found no usable adapter at runtime — check your GPU driver /
`vulkaninfo` / that you are not in a headless RDP session that hides the GPU. The
result is still correct (real CPU compute); it just did not run on the GPU.

### Quick verify (unit level)

```powershell
pwsh build/ffi.ps1 -Action test -Gpu      #  or:  task test:gpu
```

runs `daemon/ffi` + `daemon/gpu` under `-tags "ffi ffigpu"` against the
`--features gpu` staticlib. The `daemon/gpu` numerics tests pass on whichever
backend is present; on a GPU box they exercise the real wgpu path end to end.

---

## Cross-node GPU dispatch (`--on <peer>`) — status

**Available as a tested mesh primitive; composed-daemon wiring is the next step.**

The mesh transport for "run this kernel on a *peer's* GPU, capability-gated" is
implemented and unit-tested in
[`daemon/mesh/gpu.go`](../daemon/mesh/gpu.go) (`ServeGpuSigned` /
`RequestGpuSigned`), mirroring the signed WASM compute path
([`daemon/mesh/compute.go`](../daemon/mesh/compute.go)):

- The kernel + input buffers travel over a point-to-point libp2p/QUIC stream
  (`/cerberus/gpu/1.0.0`), encrypted and PeerID-authenticated by the handshake.
- The worker **verifies an Ed25519-signed capability** (`RightExec` on a GPU
  resource) against the granting node's issuer key — exchanged out of band at
  discovery — **before any kernel runs**. Same fail-closed gate as signed compute:
  an unknown issuer, a revoked cap, a missing right, or a tampered envelope is
  denied before the handler is reached. See `daemon/mesh/gpu_test.go`.
- The worker runs the kernel on **its own** best backend and returns the result
  buffer **plus the backend name it actually used**, so the requester can report
  truthfully whether the peer used `gpu-wgpu` or fell back to `cpu-software`.

**What is not wired yet (the honest next step):** the shipping `cerberusd`
composition does not register `ServeGpuSigned`, and — unlike the `test/e2e/node`
demo — it does not yet exchange issuer trust anchors at discovery. Those two are
the prerequisites for a composed `cerberus gpu <kernel> … --on <peer>` end to end.
The plan is a direct mirror of the WASM path that already ships in the e2e demo
(`test/e2e/node/node.go`: `ServeComputeSigned` + `grantExecCap` + `resolveIssuerKey`
at discovery, driven by `RequestComputeSigned`):

1. In `cmd/cerberusd` composition, call `fabric.ServeGpuSigned(handler, …)` with a
   handler that calls `daemon/gpu.Dispatch` and returns the result + backend name.
2. Reuse the discovery issuer-key exchange the e2e node already implements so the
   composed daemon holds a trusted issuer key per peer (the `resolveIssuer` gate).
3. Add an `On string` field to `GpuDispatchRequest`/`DaemonRPC.GpuDispatch`
   (exactly like `RunRequest.On`) and a `--on <peer>` flag to `cmdGPU`, routing
   through `RequestGpuSigned` when set.

This was deliberately left as documented scope rather than shipped half-working:
the mesh primitive is complete and safe on its own, and the composition wiring
depends on cross-node issuer-trust plumbing that today lives only in the e2e
harness.

---

## Files

| file | role |
|---|---|
| `core/runtime/src/gpu/wgpu_backend.rs` | the real wgpu compute backend (behind the `gpu` feature) |
| `core/cabi/src/lib.rs` | `cerberus_gpu_submit` / `cerberus_gpu_backend` C-ABI (honest backend string) |
| `core/cabi/Cargo.toml` | the `gpu` feature (forwards to `cerberus-runtime/gpu`) |
| `daemon/gpu/` | the daemon's dispatch surface (software default; `-tags ffi` routes to Rust) |
| `daemon/ffi/gpu_ffi.go` | cgo binding to `cerberus_gpu_submit` (`-tags ffi`) |
| `daemon/ffi/gpu_ffi_ld.go` | extra Win32 link flags for the GPU build (`-tags "ffi ffigpu"`) |
| `daemon/mesh/gpu.go` | cross-node GPU dispatch primitive (capability-gated) |
| `build/ffi.ps1` | `-Gpu` builds the `--features gpu` staticlib + `-tags "ffi ffigpu"` daemon |
