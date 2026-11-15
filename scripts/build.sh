#!/usr/bin/env bash
# Cross-compile rocket for Windows, macOS and Linux (amd64 + arm64) and the
# tray app for Windows and Linux (amd64 + arm64), zip each target with
# README.md, HOW-TO-OPEN.txt and the license files into dist/, build the
# signed Android app as dist/rocket-android.apk, and write dist/SHA256SUMS.
#
# The APK needs ANDROID_HOME (or ANDROID_SDK_ROOT), the gomobile toolchain
# (see scripts/build-android.sh) and the release keystore properties file
# ($ROCKET_KEYSTORE_PROPS, else ~/.rocket-signing/keystore.properties).
# Without the SDK or the keystore it is skipped with a warning; with RELEASE=1
# that is an error. Only Gradle reads the keystore; this script just checks
# that the properties file exists.
#
# Usage: bash scripts/build.sh            (VERSION defaults to git describe)
#        VERSION=1.2.0 bash scripts/build.sh
#        RELEASE=1 VERSION=1.2.0 bash scripts/build.sh   (the APK is required)
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

# The Android APK is built when the SDK and the keystore properties are there.
# Decide up front, so RELEASE=1 fails before the long desktop build.
ANDROID_SDK="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-}}"
KEYSTORE_PROPS="${ROCKET_KEYSTORE_PROPS:-$HOME/.rocket-signing/keystore.properties}"
APK_SKIP=""
if [ -z "$ANDROID_SDK" ] || [ ! -d "$ANDROID_SDK" ]; then
  APK_SKIP="ANDROID_HOME is not set to an Android SDK"
elif [ ! -f "$KEYSTORE_PROPS" ]; then
  APK_SKIP="no keystore properties file (set ROCKET_KEYSTORE_PROPS or create ~/.rocket-signing/keystore.properties)"
fi
if [ -n "$APK_SKIP" ] && [ "${RELEASE:-}" = 1 ]; then
  echo "error: RELEASE=1 needs rocket-android.apk, but $APK_SKIP" >&2
  exit 1
fi

# build_apk: build the signed release APK into $DIST/rocket-android.apk, or
# warn and skip when the SDK or the keystore is missing.
build_apk() {
  if [ -n "$APK_SKIP" ]; then
    echo "WARNING: skipping rocket-android.apk: $APK_SKIP." >&2
    return 0
  fi
  local sdk="$ANDROID_SDK"
  local out="android/app/build/outputs/apk/release"
  rm -rf "$out"
  echo "building rocket-android.apk (signed release)"
  bash scripts/build-android.sh assembleRelease
  # Gradle names an APK it could not sign app-release-unsigned.apk.
  [ -f "$out/app-release.apk" ] || { echo "error: no signed APK in $out" >&2; exit 1; }
  local bt apksigner
  bt="$(ls -1 "$sdk/build-tools" 2>/dev/null | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)"
  apksigner="$sdk/build-tools/$bt/apksigner"
  [ -f "$apksigner.bat" ] && apksigner="$apksigner.bat"
  if [ -n "$bt" ] && [ -f "$apksigner" ]; then
    "$apksigner" verify "$out/app-release.apk" || { echo "error: apksigner rejected the APK" >&2; exit 1; }
  else
    echo "WARNING: apksigner not found under $sdk/build-tools; signature not verified." >&2
  fi
  cp "$out/app-release.apk" "$DIST/rocket-android.apk"
  echo "wrote $DIST/rocket-android.apk"
}

mkdir -p "$DIST"
rm -f "$DIST"/rocket-*.zip "$DIST"/rocket-android.apk "$DIST"/SHA256SUMS
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

build_apk

# The zips first, then the APK when there is one (scripts/build.ps1 matches).
apk=""
[ -f "$DIST/rocket-android.apk" ] && apk="rocket-android.apk"
(cd "$DIST" && sha256 rocket-*.zip $apk > SHA256SUMS)
echo "wrote $DIST/SHA256SUMS"
