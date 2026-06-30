#!/usr/bin/env pwsh
# build/release.ps1 — Windows-local cross-compile + packaging for Cerberus.
# PowerShell sibling of build/release.sh (same outputs, same layout). Produces,
# under build/dist/:
#   * per-OS/arch archives  (cerberus_<ver>_<os>_<arch>.{zip,tar.gz})
#   * checksums.txt         (sha256 over every archive)
#   * update-manifest.json  (auto-update manifest SCAFFOLD; signature empty)
#
# This is the pure-Go release path (CGO_ENABLED=0), matching .goreleaser.yaml.
# It does NOT sign / notarize — that is CI scaffolding (release.yml). The real
# Rust-kernel (-tags ffi) build stays in build/ffi.ps1 and is out of scope here.
#
# Usage:
#   pwsh build/release.ps1 [-Version v0.1.0] [-OS linux] [-Arch arm64] [-ManifestOnly]
#
#   -Version       release version (default: `git describe` or 0.0.0-dev)
#   -OS / -Arch    build a single GOOS/GOARCH (default: full matrix)
#   -ManifestOnly  skip building; regenerate checksums + manifest from dist/
param(
    [string]$Version = '',
    [string]$OS = '',
    [string]$Arch = '',
    [switch]$ManifestOnly
)
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot   # build/ is one level under repo root
$dist = Join-Path $root 'build\dist'

# default full matrix
$matrixPairs = @(
    @('linux', 'amd64'), @('linux', 'arm64'),
    @('darwin', 'amd64'), @('darwin', 'arm64'),
    @('windows', 'amd64'), @('windows', 'arm64')
)

if (-not $Version) {
    try { $Version = (& git -C $root describe --tags --always --dirty).Trim() } catch { $Version = '' }
    if (-not $Version) { $Version = '0.0.0-dev' }
}
$verClean = $Version -replace '^v', ''

function Get-Sha256([string]$path) {
    return (Get-FileHash -Algorithm SHA256 -Path $path).Hash.ToLower()
}

function Build-One([string]$goos, [string]$goarch) {
    $stageName = "cerberus_${verClean}_${goos}_${goarch}"
    $stage = Join-Path $dist $stageName
    $ext = if ($goos -eq 'windows') { '.exe' } else { '' }

    Write-Host "release: building $goos/$goarch..."
    New-Item -ItemType Directory -Force -Path $stage | Out-Null

    $env:CGO_ENABLED = '0'
    $env:GOOS = $goos
    $env:GOARCH = $goarch
    foreach ($bin in @('cerberus', 'cerberusd')) {
        & go build -trimpath -ldflags '-s -w' -o (Join-Path $stage "$bin$ext") (Join-Path $root "cmd\$bin")
        if ($LASTEXITCODE -ne 0) { throw "go build failed for $bin ($goos/$goarch)" }
    }

    foreach ($doc in @('README.md', 'ARCHITECTURE.md', 'CONTRACT.md', 'LICENSE')) {
        $src = Join-Path $root $doc
        if (Test-Path $src) { Copy-Item $src $stage }
    }

    # Archive: zip for Windows, tar.gz elsewhere (tar ships with Windows 10+).
    Push-Location $dist
    try {
        if ($goos -eq 'windows') {
            Compress-Archive -Path $stageName -DestinationPath "$stageName.zip" -Force
        }
        else {
            & tar -czf "$stageName.tar.gz" $stageName
            if ($LASTEXITCODE -ne 0) { throw "tar failed for $stageName" }
        }
    }
    finally { Pop-Location }
    Remove-Item -Recurse -Force $stage
}

# ---------------------------------------------------------------- build phase
if (-not $ManifestOnly) {
    New-Item -ItemType Directory -Force -Path $dist | Out-Null
    if ($OS -or $Arch) {
        if (-not ($OS -and $Arch)) { throw 'release.ps1: -OS and -Arch must be given together' }
        Build-One $OS $Arch
    }
    else {
        foreach ($p in $matrixPairs) { Build-One $p[0] $p[1] }
    }
}

# ---------------------------------------------------------------- checksums
New-Item -ItemType Directory -Force -Path $dist | Out-Null
$archives = Get-ChildItem -Path $dist -File | Where-Object { $_.Name -match '\.(tar\.gz|zip)$' }
$checksumLines = foreach ($a in $archives) { "{0}  {1}" -f (Get-Sha256 $a.FullName), $a.Name }
Set-Content -Path (Join-Path $dist 'checksums.txt') -Value $checksumLines -Encoding ascii
Write-Host "release: wrote $dist\checksums.txt"

# ---------------------------------------------------------------- auto-update manifest (SCAFFOLD)
# REAL: lists every artifact with sha256 + size for client-side verification.
# SCAFFOLD: `signature` is empty — a real channel signs the manifest and serves
# it from an update endpoint (see release.yml). An empty signature is NOT signed.
$artifacts = foreach ($a in $archives) {
    [ordered]@{ name = $a.Name; sha256 = (Get-Sha256 $a.FullName); size = $a.Length }
}
$manifest = [ordered]@{
    schema           = 'cerberus.update/v1'
    version          = $verClean
    released_at      = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    signature        = ''
    _signature_note  = 'SCAFFOLD: unsigned. A real update channel signs this manifest (minisign/cosign); wire the key + endpoint in .github/workflows/release.yml.'
    artifacts        = @($artifacts)
}
$manifest | ConvertTo-Json -Depth 5 | Set-Content -Path (Join-Path $dist 'update-manifest.json') -Encoding ascii
Write-Host "release: wrote $dist\update-manifest.json (auto-update SCAFFOLD — signature empty by design)"

Write-Host "release: done. Artifacts in $dist"
