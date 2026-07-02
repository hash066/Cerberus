#!/usr/bin/env pwsh
# build/ffi.ps1 — build & test the Go<->Rust cgo binding (`-tags ffi`), which
# replaces the default pure-Go stub kernel with the real Rust SignedKernel
# (Ed25519-signed capabilities). See docs/ffi.md for the full rationale.
#
# Why this script exists: cgo on Windows needs a GCC-compatible C compiler (it
# cannot drive MSVC cl.exe), and the Rust core must be a matching GNU-ABI
# staticlib. This script:
#   1. builds core/cabi as an x86_64-pc-windows-gnu staticlib,
#   2. removes the single compiler_builtins object that duplicates zig's
#      compiler_rt `__chkstk_ms` (COFF lld has no --allow-multiple-definition),
#   3. builds/tests against it with CGO_ENABLED=1, GOARCH=amd64, CC="zig cc".
#
# REAL GPU (-Gpu): pass -Gpu to build the staticlib WITH cabi's `gpu` feature, so
# cerberus_gpu_submit runs on the physical GPU via wgpu (falling back to the Rust
# software backend only when no adapter is present). Without -Gpu the staticlib
# ships the software GpuDispatch (real host compute), and `cerberus gpu` reports
# backend `cpu-software`. With -Gpu on a machine with a working GPU/driver (e.g. an
# NVIDIA RTX card) it reports `gpu-wgpu`. See docs/gpu.md for the end-to-end steps.
#
# Requirements (one-time):
#   * zig            -> set $env:ZIG to zig.exe, or put `zig` on PATH
#   * rustup target add x86_64-pc-windows-gnu
#   * rustup component add llvm-tools-preview     (gives llvm-ar / llvm-nm)
#
# Usage:  pwsh build/ffi.ps1 [-Action test|build|run] [-Gpu]
param(
    [ValidateSet('test', 'build', 'run')] [string]$Action = 'test',
    [switch]$Gpu
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot   # build/ is one level under repo root

# --- locate the GNU C toolchain (zig) ---------------------------------------
$zig = if ($env:ZIG) { $env:ZIG } else { (Get-Command zig -ErrorAction SilentlyContinue).Source }
if (-not $zig) { throw "zig not found. Set `$env:ZIG to zig.exe (a space-free path) or add zig to PATH. See docs/ffi.md." }
if ($zig -match '\s') { throw "zig path '$zig' contains spaces; cgo CC parsing needs a space-free path. Move zig or symlink it." }

function Find-LlvmTool($name) {
    $c = Get-Command $name -ErrorAction SilentlyContinue
    if ($c) { return $c.Source }
    $tc = (rustup show active-toolchain) -split ' ' | Select-Object -First 1
    $p = Join-Path $env:USERPROFILE ".rustup\toolchains\$tc\lib\rustlib\x86_64-pc-windows-msvc\bin\$name.exe"
    if (Test-Path $p) { return $p }
    throw "$name not found. Run: rustup component add llvm-tools-preview"
}
$llvmar = Find-LlvmTool 'llvm-ar'
$llvmnm = Find-LlvmTool 'llvm-nm'

Push-Location $root
try {
    # --- 1. build the GNU-ABI staticlib (staticlib only: no external linker) ---
    # -Gpu forwards cabi's `gpu` feature so cerberus_gpu_submit binds the real wgpu
    # device; without it the staticlib keeps the software GpuDispatch (see docs/gpu.md).
    $featureArgs = @()
    if ($Gpu) { $featureArgs = @('--features', 'gpu') }
    if ($Gpu) {
        Write-Host "ffi: building cerberus-cabi (x86_64-pc-windows-gnu staticlib, --features gpu -> real wgpu)..."
    } else {
        Write-Host "ffi: building cerberus-cabi (x86_64-pc-windows-gnu staticlib)..."
    }
    & cargo rustc -p cerberus-cabi --target x86_64-pc-windows-gnu --crate-type staticlib @featureArgs
    if ($LASTEXITCODE -ne 0) { throw "cargo build failed" }
    $lib = Join-Path $root 'target\x86_64-pc-windows-gnu\debug\libcerberus_cabi.a'

    # --- 2. drop the duplicate __chkstk_ms object (zig's compiler_rt owns it) ---
    $member = & $llvmnm --print-armap $lib 2>$null |
        Select-String 'chkstk_ms in ' |
        ForEach-Object { ($_.Line -split ' in ', 2)[1].Trim() } |
        Select-Object -Unique -First 1
    if ($member) {
        & $llvmar d $lib $member
        Write-Host "ffi: removed duplicate builtin object $member"
    }

    # --- 3. build/test the cgo path -------------------------------------------
    # With -Gpu we also enable the `ffigpu` build tag so daemon/ffi links wgpu's
    # extra Win32 system libs (gpu_ffi_ld.go). The staticlib built in step 1 must
    # match: -Gpu here REQUIRES the `--features gpu` staticlib built above.
    $env:CGO_ENABLED = '1'
    $env:GOARCH = 'amd64'
    $env:CC = "$zig cc"
    $tags = if ($Gpu) { 'ffi ffigpu' } else { 'ffi' }
    switch ($Action) {
        'test' { & go test -tags $tags ./daemon/ffi/ ./daemon/gpu/ -v }
        'build' { & go build -tags $tags -o (Join-Path $root 'cerberusd-ffi.exe') ./cmd/cerberusd; Write-Host "ffi: built cerberusd-ffi.exe" }
        'run' { & go run -tags $tags ./cmd/cerberusd }
    }
    if ($LASTEXITCODE -ne 0) { throw "go $Action failed" }
}
finally {
    Pop-Location
}
