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

**Wired in the composed daemon (v0.1).**

The mesh transport for "run this kernel on a *peer's* GPU, capability-gated" is
implemented in
[`daemon/mesh/gpu.go`](../daemon/mesh/gpu.go) (`ServeGpuSigned` /
`RequestGpuSigned`), mirroring the signed WASM compute path
([`daemon/mesh/compute.go`](../daemon/mesh/compute.go)):

- The kernel + input buffers travel over a point-to-point libp2p/QUIC stream
  (`/cerberus/gpu/1.0.0`), encrypted and PeerID-authenticated by the handshake.
- The worker **verifies an Ed25519-signed capability** (`RightExec` on
  `mesh.MeshGpuResource(site)`) with **byte/FLOP quota bounds** before any kernel
  runs. Same fail-closed gate as signed compute.
- The worker runs the kernel on **its own** best backend and returns the result
  buffer **plus the backend name it actually used**.

**Composed daemon wiring (shipped):**

1. `system.Compose` calls `gpu.WireWorker` → `fabric.ServeGpuSigned` with a
   handler that calls `daemon/gpu.Dispatch` and enforces grant quotas.
2. Production daemons use the **self-issued** mesh identity trust model
   (`mesh.SelfIssuerResolver`) — same as mesh compute, shards, and audio.
3. `DaemonRPC.GpuDispatch` and `cerberus gpu … --on <peer>` route through
   `gpu.DispatchRemote` → `RequestGpuSigned` when `--on` is set.
4. Peer VRAM telemetry is consumed by the scheduler via
   `system.schedulerLoop` (subscribes to `cerberus/<site>/telemetry/**`) and
   exposed in the tray status API as `gpu_pool`.

---

## Mount a peer's GPU as a device (`/cer/dev/gpu`) — the 9P + data-plane path

Besides the point-to-point `--on <peer>` RPC above, a node's GPU is exposed as a
**capability-addressed 9P device** — the "mount a remote GPU as a file" model
(vertical 04, ARCHITECTURE §4.1). This is the control-plane/data-plane split done
properly:

```
holder                          serving node
  │  9P walk /cer/dev/gpu/local/0        (cap-checked: read)
  │  9P open .../ctl                     (cap-checked: alloc)
  │        └──> mints a data-plane GRANT (a QUIC transfer id + quota) ──┐
  │                                                                     ▼
  │  dial the granted QUIC data-plane session ───────────►  dataplane.Responder
  │  send f32 kernel request  ──────────────────────────►  gpu.Serve → Dispatch
  │  ◄──────────────────────────────  result + backend name (same session)
```

- **9P is control-only.** Walking the device and opening `ctl` are each
  capability-checked (`daemon/ninep`). No kernel byte ever crosses the 9P wire
  (vertical 04 §3.5); opening `ctl` returns a **data-plane endpoint**.
- **The data plane carries the work.** The grant is a real
  capability+quota-bound QUIC transfer (`daemon/dataplane`). A `KindGPU` device
  registers a `dataplane.Responder` (`Server.RegisterResponder`) instead of the
  one-way byte-drain a VRAM/bulk grant uses: the request blob is decoded by
  `gpu.Serve`, run through `gpu.Dispatch`, and the result is returned on the same
  session. `gpu.RequestSession` is the requester side.
- **Same honest backend string.** The session reports `gpu-wgpu` or
  `cpu-software` exactly like the CLI/mesh paths — a peer that fell back to
  software says so.

Wired in `daemon/system.Compose` (the `SetGranter` closure branches on
`ref.Kind == KindGPU`). Proven end-to-end (no GPU hardware needed, default
software backend) by `TestComposeGpuDeviceRunsKernelOverDataPlane`
(`daemon/system`) and `TestGpuOverNinePDataPlaneSession` (`daemon/gpu`).

## Scheduler placement of GPU/VRAM-bound work

`scheduler.PlaceGPU(taskID, minVRAM)` places a GPU/VRAM-bound task on the node
with the most **free VRAM** meeting `minVRAM`, read from live telemetry
(`NodeTelemetry.Memory.VRAMFree`), skipping thermally-throttling nodes; the
next-best node becomes a hot standby, and the plan participates in the same
`Reroute`/`RerouteNode` machinery as CPU placement. Peer VRAM telemetry reaches
the scheduler via `system.schedulerLoop` (subscribed to
`cerberus/<site>/telemetry/**`).

### Honest remaining gaps (not faked, not §8 Frontier)

- **Live VRAM quota accounting.** `PlaceGPU` reads free VRAM to *choose* a node,
  but the composed daemon does not yet *debit* a node's live VRAM as device
  sessions run, nor auto-route an opened device to the placement result. Those
  accounting/auto-routing steps remain.
- **RDMA / true zero-copy** for the GPU data-plane session is Frontier (vertical
  04 §10); the session streams over QUIC (the "zero-copy intent"), not RDMA.
- **zk-WASM proof-of-inference** and **host-TEE memory shielding** stay documented
  stubs (ARCHITECTURE §8) — nothing here fakes them.

---

## Files

| file | role |
|---|---|
| `core/runtime/src/gpu/wgpu_backend.rs` | the real wgpu compute backend (behind the `gpu` feature) |
| `core/cabi/src/lib.rs` | `cerberus_gpu_submit` / `cerberus_gpu_backend` C-ABI (honest backend string) |
| `core/cabi/Cargo.toml` | the `gpu` feature (forwards to `cerberus-runtime/gpu`) |
| `daemon/gpu/` | the daemon's dispatch surface (software default; `-tags ffi` routes to Rust) |
| `daemon/gpu/session.go` | GPU-over-data-plane session codec (`Serve` / `RequestSession`) for the 9P `/cer/dev/gpu` device path |
| `daemon/dataplane/server.go` | `RegisterResponder` — the request/response session a GPU ctl grant uses |
| `daemon/ffi/gpu_ffi.go` | cgo binding to `cerberus_gpu_submit` (`-tags ffi`) |
| `daemon/ffi/gpu_ffi_ld.go` | extra Win32 link flags for the GPU build (`-tags "ffi ffigpu"`) |
| `daemon/mesh/gpu.go` | cross-node GPU dispatch primitive (capability-gated) |
| `build/ffi.ps1` | `-Gpu` builds the `--features gpu` staticlib + `-tags "ffi ffigpu"` daemon |
