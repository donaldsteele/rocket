#!/usr/bin/env bash
# Build the Go core of the Android app (mobile/rocketcore) into
# android/app/libs/rocketcore.aar with gomobile bind, then run Gradle in
# android/ when the app project is there.
#
# Needs Go, gomobile and gobind (go install golang.org/x/mobile/cmd/gomobile
# and .../gobind, then gomobile init), and an Android SDK with an NDK.
# ANDROID_HOME defaults to the usual SDK folder of the OS (ANDROID_SDK_ROOT is
# also read); ANDROID_NDK_HOME defaults to the newest NDK under
# $ANDROID_HOME/ndk.
#
# Usage: bash scripts/build-android.sh              (AAR, then gradlew assembleDebug)
#        bash scripts/build-android.sh --aar-only
#        bash scripts/build-android.sh testDebugUnitTest lintDebug assembleDebug
set -euo pipefail
cd "$(dirname "$0")/.."

aar_only=0
tasks=()
for a in "$@"; do
  case "$a" in
    --aar-only) aar_only=1 ;;
    *) tasks+=("$a") ;;
  esac
done
[ ${#tasks[@]} -gt 0 ] || tasks=(assembleDebug)

sdk="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-}}"
if [ -z "$sdk" ]; then
  case "$(uname -s)" in
    Darwin) sdk="$HOME/Library/Android/sdk" ;;
    MINGW*|MSYS*|CYGWIN*) sdk="${LOCALAPPDATA:-}/Android/Sdk" ;;
    *) sdk="$HOME/Android/Sdk" ;;
  esac
fi
[ -d "$sdk" ] || { echo "Android SDK not found at $sdk (set ANDROID_HOME)" >&2; exit 1; }
export ANDROID_HOME="$sdk"

ndk="${ANDROID_NDK_HOME:-}"
if [ -z "$ndk" ] && [ -d "$sdk/ndk" ]; then
  newest="$(ls -1 "$sdk/ndk" | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)"
  [ -n "$newest" ] && ndk="$sdk/ndk/$newest"
fi
[ -n "$ndk" ] && [ -d "$ndk" ] || { echo "Android NDK not found (set ANDROID_NDK_HOME)" >&2; exit 1; }
export ANDROID_NDK_HOME="$ndk"

# gomobile runs gobind from PATH; both live in GOPATH/bin after go install.
export PATH="$(go env GOPATH)/bin:$PATH"
command -v gomobile >/dev/null || { echo "gomobile not found (go install golang.org/x/mobile/cmd/gomobile@latest; gomobile init)" >&2; exit 1; }
command -v gobind >/dev/null || { echo "gobind not found (go install golang.org/x/mobile/cmd/gobind@latest)" >&2; exit 1; }

aar="android/app/libs/rocketcore.aar"
mkdir -p "$(dirname "$aar")"
echo "gomobile bind -> $aar (NDK $(basename "$ndk"))"
gomobile bind \
  -target=android/arm64,android/arm,android/amd64 \
  -androidapi 26 \
  -javapkg io.github.donaldsteele.rocket.core \
  -trimpath -ldflags="-s -w" \
  -o "$aar" \
  ./mobile/rocketcore

[ "$aar_only" = 1 ] && exit 0
if [ ! -f android/gradlew ]; then
  echo "android/ has no Gradle project yet; built the AAR only."
  exit 0
fi
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) gradlew="./gradlew.bat" ;;
  *) gradlew="./gradlew" ;;
esac
(cd android && "$gradlew" "${tasks[@]}")
