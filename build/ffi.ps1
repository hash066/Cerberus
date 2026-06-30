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
# Requirements (one-time):
#   * zig            -> set $env:ZIG to zig.exe, or put `zig` on PATH
#   * rustup target add x86_64-pc-windows-gnu
#   * rustup component add llvm-tools-preview     (gives llvm-ar / llvm-nm)
#
# Usage:  pwsh build/ffi.ps1 [-Action test|build|run]
param(
    [ValidateSet('test', 'build', 'run')] [string]$Action = 'test'
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
    Write-Host "ffi: building cerberus-cabi (x86_64-pc-windows-gnu staticlib)..."
    & cargo rustc -p cerberus-cabi --target x86_64-pc-windows-gnu --crate-type staticlib
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
    $env:CGO_ENABLED = '1'
    $env:GOARCH = 'amd64'
    $env:CC = "$zig cc"
    switch ($Action) {
        'test' { & go test -tags ffi ./daemon/ffi/ -v }
        'build' { & go build -tags ffi -o (Join-Path $root 'cerberusd-ffi.exe') ./cmd/cerberusd; Write-Host "ffi: built cerberusd-ffi.exe" }
        'run' { & go run -tags ffi ./cmd/cerberusd }
    }
    if ($LASTEXITCODE -ne 0) { throw "go $Action failed" }
}
finally {
    Pop-Location
}
