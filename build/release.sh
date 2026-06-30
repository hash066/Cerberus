#!/usr/bin/env bash
# build/release.sh — cross-compile + package the Cerberus binaries locally and
# in CI. Produces, under build/dist/:
#   * per-OS/arch archives  (cerberus_<ver>_<os>_<arch>.{tar.gz,zip})
#   * checksums.txt         (sha256 over every archive)
#   * update-manifest.json  (auto-update manifest SCAFFOLD — see note below)
#
# This is the pure-Go release path (CGO_ENABLED=0), matching build/.goreleaser.yaml.
# It does NOT sign or notarize — that is CI scaffolding (release.yml) where real
# certs plug in. The optional `-tags ffi` Rust-kernel build stays Windows-local
# via build/ffi.ps1 and is intentionally out of the portable cross-build.
#
# Usage:
#   build/release.sh [--version vX.Y.Z] [--os GOOS] [--arch GOARCH] [--manifest-only]
#
#   --version       release version string (default: `git describe` or 0.0.0-dev)
#   --os / --arch   build a single GOOS/GOARCH (default: full matrix below)
#   --manifest-only skip building; regenerate checksums + manifest from existing dist/
#
# Examples:
#   build/release.sh --version v0.1.0                 # full matrix
#   build/release.sh --version v0.1.0 --os linux --arch arm64
#   build/release.sh --version v0.1.0 --manifest-only
set -euo pipefail

# repo root = parent of this script's dir
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DIST="$ROOT/build/dist"

# default full matrix (os:arch)
MATRIX=(
  "linux:amd64" "linux:arm64"
  "darwin:amd64" "darwin:arm64"
  "windows:amd64" "windows:arm64"
)

VERSION=""
ONLY_OS=""
ONLY_ARCH=""
MANIFEST_ONLY=0

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --os) ONLY_OS="$2"; shift 2 ;;
    --arch) ONLY_ARCH="$2"; shift 2 ;;
    --manifest-only) MANIFEST_ONLY=1; shift ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "release.sh: unknown arg '$1'" >&2; exit 2 ;;
  esac
done

if [ -z "$VERSION" ]; then
  VERSION="$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")"
fi
# strip a leading 'v' for clean archive names
VER_CLEAN="${VERSION#v}"

# sha256 helper: prefer sha256sum (Linux), fall back to shasum (macOS).
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

build_one() {
  local goos="$1" goarch="$2"
  local stage="$DIST/cerberus_${VER_CLEAN}_${goos}_${goarch}"
  local ext=""
  [ "$goos" = "windows" ] && ext=".exe"

  echo "release: building ${goos}/${goarch}..."
  mkdir -p "$stage"

  # Pure-Go, reproducible-ish flags (mirror .goreleaser.yaml).
  for bin in cerberus cerberusd; do
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath -ldflags "-s -w" \
      -o "$stage/${bin}${ext}" "$ROOT/cmd/${bin}"
  done

  # Drop docs alongside the binaries (best-effort; never fail the build on them).
  for doc in README.md ARCHITECTURE.md CONTRACT.md LICENSE; do
    [ -f "$ROOT/$doc" ] && cp "$ROOT/$doc" "$stage/" || true
  done

  # Archive: zip for Windows, tar.gz elsewhere.
  ( cd "$DIST"
    local base; base="$(basename "$stage")"
    if [ "$goos" = "windows" ]; then
      if command -v zip >/dev/null 2>&1; then
        zip -qr "${base}.zip" "$base"
      else
        # zip absent (some minimal CI images): fall back to tar.gz so we still
        # emit an artifact rather than silently producing nothing.
        echo "release: 'zip' not found; emitting ${base}.tar.gz instead" >&2
        tar -czf "${base}.tar.gz" "$base"
      fi
    else
      tar -czf "${base}.tar.gz" "$base"
    fi
  )
  rm -rf "$stage"
}

# ---------------------------------------------------------------- build phase
if [ "$MANIFEST_ONLY" -eq 0 ]; then
  mkdir -p "$DIST"
  if [ -n "$ONLY_OS" ] || [ -n "$ONLY_ARCH" ]; then
    [ -n "$ONLY_OS" ] && [ -n "$ONLY_ARCH" ] || { echo "release.sh: --os and --arch must be given together" >&2; exit 2; }
    build_one "$ONLY_OS" "$ONLY_ARCH"
  else
    for pair in "${MATRIX[@]}"; do
      build_one "${pair%%:*}" "${pair##*:}"
    done
  fi
fi

# ---------------------------------------------------------------- checksums
mkdir -p "$DIST"
( cd "$DIST"
  : > checksums.txt
  shopt -s nullglob
  for f in *.tar.gz *.zip; do
    echo "$(sha256_of "$f")  $f" >> checksums.txt
  done
)
echo "release: wrote $DIST/checksums.txt"

# ---------------------------------------------------------------- auto-update manifest (SCAFFOLD)
# A minimal, self-describing update manifest. REAL: it lists every artifact with
# its sha256 + size so a client can verify a download. SCAFFOLD: the `signature`
# field is empty — a real channel signs the manifest (e.g. Tauri updater minisign
# / cosign) and serves it from an update endpoint. release.yml documents where
# that key + endpoint plug in. Do NOT treat the empty signature as "signed".
MANIFEST="$DIST/update-manifest.json"
NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
{
  echo "{"
  echo "  \"schema\": \"cerberus.update/v1\","
  echo "  \"version\": \"${VER_CLEAN}\","
  echo "  \"released_at\": \"${NOW}\","
  echo "  \"signature\": \"\","
  echo "  \"_signature_note\": \"SCAFFOLD: unsigned. A real update channel signs this manifest (minisign/cosign); wire the key + endpoint in .github/workflows/release.yml.\","
  echo "  \"artifacts\": ["
  first=1
  ( cd "$DIST"
    shopt -s nullglob
    for f in *.tar.gz *.zip; do
      sum="$(sha256_of "$f")"
      # portable file size (Linux stat -c, macOS stat -f)
      size="$(stat -c%s "$f" 2>/dev/null || stat -f%z "$f")"
      if [ "$first" -eq 1 ]; then first=0; else echo ","; fi
      printf '    {"name": "%s", "sha256": "%s", "size": %s}' "$f" "$sum" "$size"
    done
    echo ""
  )
  echo "  ]"
  echo "}"
} > "$MANIFEST"
echo "release: wrote $MANIFEST (auto-update SCAFFOLD — signature empty by design)"

echo "release: done. Artifacts in $DIST"
