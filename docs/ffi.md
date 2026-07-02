# Binding the real Rust OCap kernel via cgo (`-tags ffi`)

By default `cerberusd` uses a **pure-Go stub** capability kernel
([daemon/ffi/kernel.go](../daemon/ffi/kernel.go), `Backend() == "pure-go-stub"`),
so the daemon builds and tests with **no C toolchain** (`CGO_ENABLED=0`). This
keeps the day-to-day build trivial.

Building with **`-tags ffi`** swaps in the real Rust capability kernel over cgo
([daemon/ffi/kernel_ffi.go](../daemon/ffi/kernel_ffi.go) →
[core/cabi](../core/cabi) → [core/ocap](../core/ocap) `SignedKernel`).
Capabilities then become **cryptographically enforced end to end**: every
`Mint`/`Attenuate`/`Verify`/`Revoke` the Go control plane performs is an
Ed25519-signed CBOR capability with a verified attenuation chain. `Backend()`
reports `rust-signed-cabi`, and the daemon's startup banner shows
`kernel=rust-signed-cabi`.

This is **Phase E item 1** in [HANDOFF.md](../HANDOFF.md).

## Why it needs a special toolchain on Windows

Two hard constraints drive the recipe:

1. **cgo cannot drive MSVC `cl.exe`.** Go's cgo only speaks to a
   GCC-compatible C compiler (gcc/clang). The machine has MSVC (that's what
   Rust links with) but no gcc/clang, so we use **`zig cc`** (portable, no
   admin) as the C compiler/linker.
2. **The Rust staticlib must share the GNU ABI.** `zig cc` links in
   `x86_64-windows-gnu` mode, so the Rust core is built for the
   **`x86_64-pc-windows-gnu`** target (not the default msvc), and the whole
   `-tags ffi` build targets **`GOARCH=amd64`** (the daemon's default build is
   `windows/386`).

One overlap remains: Rust's bundled `compiler_builtins` and zig's `compiler_rt`
both define `__chkstk_ms` (a stack-probe routine). COFF `lld-link` has no
`--allow-multiple-definition`, so we delete the single, self-contained
`compiler_builtins …__chkstk_ms` object from the staticlib and let zig's
`compiler_rt` provide it. Rust's gnu std also needs the unwinder
(`_Unwind_*`), supplied by zig's bundled `libunwind` (`-lunwind`).

## One-time setup

```
# a GNU-compatible C compiler for cgo (portable zip, no admin):
#   download ziglang.org/download → set ZIG to the space-free path of zig.exe
rustup target add x86_64-pc-windows-gnu      # GNU-ABI Rust std
rustup component add llvm-tools-preview       # llvm-ar / llvm-nm for the patch step
```

## Build / test

```
pwsh build/ffi.ps1 -Action test     # build cabi (gnu) + patch + `go test -tags ffi ./daemon/ffi`
pwsh build/ffi.ps1 -Action build    # produce cerberusd-ffi.exe
pwsh build/ffi.ps1 -Action run      # run the daemon with the Rust kernel
# or via task:  task build:ffi   /   task test:ffi
```

Add **`-Gpu`** to build the cabi staticlib with the real wgpu GPU backend
(`--features gpu`) and the daemon with the extra `ffigpu` link tag, so
`cerberus gpu` reports `backend: gpu-wgpu` on a machine with a GPU. See
[docs/gpu.md](gpu.md) for the end-to-end steps.

```
pwsh build/ffi.ps1 -Action build -Gpu    # cerberusd-ffi.exe with the real GPU backend
# or via task:  task build:gpu  /  task test:gpu
```

The script encapsulates the three steps (build gnu staticlib → strip the
duplicate builtin → `go {test,build} -tags ffi` with `CGO_ENABLED=1`,
`GOARCH=amd64`, `CC="zig cc"`).

## The C ABI (the keystone)

The Go↔Rust seam is intentionally tiny — opaque `u64` handles only, all memory
owned Rust-side ([ARCHITECTURE.md §2.1](../ARCHITECTURE.md)). It is declared in
[core/cabi/include/cerberus.h](../core/cabi/include/cerberus.h) and implemented
in [core/cabi/src/lib.rs](../core/cabi/src/lib.rs):

| C function | Backs `contract.CapKernel` |
|---|---|
| `cerberus_cap_mint(kind, rights, quota, max_caveat, now, ttl)` | `Mint` |
| `cerberus_cap_attenuate(parent, drop_rights, max_caveat)` | `Attenuate` |
| `cerberus_cap_verify(handle, op, now)` | `Verify` |
| `cerberus_cap_revoke(handle)` | `Revoke` |
| `cerberus_cap_is_revoked(handle)` | `IsRevoked` |
| `cerberus_backend()` / `cerberus_version()` / `cerberus_issuer(out)` | introspection |

Rights cross as a bitmask, resource kinds as a small enum, and the common
`max_bytes` caveat as a `u64`; richer caveats stay Rust-side. The bitmask/enum
values are mirrored in the C header and `kernel_ffi.go` — keep the three in
sync (the C ABI is **not** the frozen contract, but a thin adapter to it).

## Non-Windows

The same idea applies with any GNU-compatible compiler (gcc/clang/zig) and the
matching Rust target; `kernel_ffi.go` already splits `#cgo` LDFLAGS by OS. The
helper script is Windows-first for now (that's the dev box in use); porting it
is just swapping the target triple and the system libs.
