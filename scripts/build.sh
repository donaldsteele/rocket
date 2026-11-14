#!/usr/bin/env bash
# Cross-compile rocket for Windows, macOS and Linux (amd64 + arm64) and the
# tray app for Windows and Linux (amd64 + arm64), zip each target with
# README.md, HOW-TO-OPEN.txt and the license files into dist/, and write
# dist/SHA256SUMS.
#
# Usage: bash scripts/build.sh            (VERSION defaults to git describe)
#        VERSION=1.2.0 bash scripts/build.sh
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
VERSION="${VERSION#v}" # rocket version prints "rocket 1.2.0", not "rocket v1.2.0"
DIST="dist"
TARGETS="windows/amd64 windows/arm64 darwin/amd64 darwin/arm64 linux/amd64 linux/arm64"
# The macOS tray needs cgo, so it is not cross-compiled (build it on a Mac).
TRAY_TARGETS="windows/amd64 windows/arm64 linux/amd64 linux/arm64"
LDPKG="github.com/donaldsteele/rocket/internal/buildinfo.Version"

DOCS="README.md HOW-TO-OPEN.txt LICENSE NOTICE THIRD_PARTY_LICENSES.txt"
for f in $DOCS; do
  [ -f "$f" ] || { echo "missing $f" >&2; exit 1; }
done

stage="$(mktemp -d)"
trap 'rm -rf "$stage" cmd/rocket/rsrc_windows_*.syso' EXIT

# make_zip <dest.zip> <dir-containing-files>  (files are added at the zip root)
# Uses tools/mkzip (pure Go) so the build is identical on every OS and the
# binaries keep their executable bit for macOS and Linux.
make_zip() {
  local dest="$1" src="$2"
  rm -f "$dest"
  "$mkzip" -o "$dest" -x rocket -x rocket.exe -x rocket-tray -x rocket-tray.exe "$src"/*
}

# sha256 <file>...  prints "<hash>  <file>" lines (GNU coreutils or macOS).
# The sed drops the binary-mode "*" that sha256sum adds on Windows, so the
# file is the same on every OS.
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  else
    shasum -a 256 "$@"
  fi | sed 's/ [*]/  /'
}

has_tray() {
  case " $TRAY_TARGETS " in *" $1 "*) return 0 ;; esac
  return 1
}

mkdir -p "$DIST"
rm -f "$DIST"/rocket-*.zip "$DIST"/SHA256SUMS
# The license file must list exactly the modules being shipped.
go run ./tools/licenses -check
mkzip="$stage/mkzip"
go build -o "$mkzip" ./tools/mkzip
echo "rocket version: $VERSION"

# Version info, manifest and icon for rocket.exe (cmd/rocket/winres). go
# build links the generated rsrc_windows_*.syso into the Windows targets only.
go tool go-winres make --in cmd/rocket/winres/winres.json --out cmd/rocket/rsrc \
  --arch amd64,arm64 --product-version "$VERSION" --file-version "$VERSION"

for target in $TARGETS; do
  goos="${target%/*}"
  goarch="${target#*/}"
  exe=""
  [ "$goos" = "windows" ] && exe=".exe"

  pkg="$stage/$goos-$goarch"
  mkdir -p "$pkg"
  echo "building $goos/$goarch"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X $LDPKG=$VERSION" \
    -o "$pkg/rocket$exe" ./cmd/rocket

  if has_tray "$target"; then
    echo "building tray $goos/$goarch"
    trayflags="-s -w -X main.version=$VERSION"
    [ "$goos" = "windows" ] && trayflags="$trayflags -H=windowsgui"
    (cd tray && CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath -ldflags "$trayflags" -o "$pkg/rocket-tray$exe" .)
  fi
  cp $DOCS "$pkg/"

  zipname="rocket-$goos-$goarch.zip"
  make_zip "$DIST/$zipname" "$pkg"
  echo "wrote $DIST/$zipname"
  rm -rf "$pkg"
done

(cd "$DIST" && sha256 rocket-*.zip > SHA256SUMS)
echo "wrote $DIST/SHA256SUMS"
