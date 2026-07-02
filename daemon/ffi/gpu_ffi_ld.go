//go:build ffi && ffigpu

// Extra link flags for the REAL-GPU cgo build (`-tags "ffi ffigpu"`), paired with
// a cabi staticlib built `--features gpu` (see build/ffi.ps1 -Gpu and docs/gpu.md).
//
// Why a separate, tag-gated file: the default `-tags ffi` build links only the
// capability-kernel + blockstore + software-GPU staticlib, whose Windows deps are
// already covered by kernel_ffi.go's LDFLAGS. Turning on cabi's `gpu` feature
// pulls wgpu into the staticlib, and wgpu's OpenGL backend (glow + glutin_wgl_sys)
// references a few extra Win32 system libraries at link time. We add ONLY those
// here, and ONLY under the `ffigpu` tag, so the plain `-tags ffi` link is byte-for-
// byte unchanged for the many users who never build the GPU path.
//
// What is NOT listed here on purpose (they need no link-time flag):
//   - Direct3D 12 / DXGI: wgpu reaches these through the `windows` crate, which
//     imports every Win32 API via `kind = "raw-dylib"`. rustc synthesizes those
//     import stubs directly into the staticlib, so no `-l` (and no import-lib
//     search path) is required for them.
//   - Vulkan (`ash`) and the D3D shader compiler (`d3dcompiler_47.dll`): wgpu
//     dlopen()s these at RUNTIME via libloading, so there is nothing to resolve at
//     link time — they only need the driver/runtime present on the machine.
//
// These three are always-present Win32 system import libraries in the mingw/zig
// sysroot; naming a lib whose symbols the final image does not actually reference
// is harmless (the linker pulls nothing from it).
package ffi

/*
#cgo windows LDFLAGS: -lopengl32 -lgdi32 -luser32
*/
import "C"
