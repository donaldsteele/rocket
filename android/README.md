# rocket for Android

Kotlin + Jetpack Compose UI over rocket's Go core (`mobile/rocketcore`,
built with `gomobile bind`). Android 8.0 (API 26) and later.

## Build

1. Android SDK with platform 36, build-tools and an NDK; JDK 17 or later.
2. Build the Go core into `app/libs/rocketcore.aar` (generated, not in git),
   from the repository root:

   ```
   pwsh scripts/build-android.ps1 -AarOnly      # or: bash scripts/build-android.sh --aar-only
   ```

3. From `android/`:

   ```
   ./gradlew testDebugUnitTest lintDebug assembleDebug     # Windows: gradlew.bat
   ```

   The debug APK lands in `app/build/outputs/apk/debug/`.

Without the AAR the build stops at `checkRocketcore` with this hint.

## Release signing

`assembleRelease` signs with the key described by `keystore.properties`
(`storeFile`, `storePassword`, `keyAlias`, `keyPassword`; a relative
`storeFile` is relative to the properties file). The build reads its path
from `ROCKET_KEYSTORE_PROPS`, else `~/.rocket-signing/keystore.properties`.
Without that file the release APK is built unsigned. The keystore and its
passwords never go into this repository or CI.

For a release, `scripts/build.sh` / `scripts/build.ps1` (repository root) run
`assembleRelease` through `scripts/build-android.*`, check the APK with
`apksigner verify --print-certs` and stop unless its signer certificate's
SHA-256 equals `android/release-cert.sha256` (the digest published in
`docs/security.md`), copy it to `dist/rocket-android.apk` (universal: arm64-v8a,
armeabi-v7a and x86_64 in one APK) and list it in `dist/SHA256SUMS`. Without
`ANDROID_HOME` or the properties file they warn and skip the APK; with
`RELEASE=1` (PowerShell: `-Release`) they stop instead. CI
(`.github/workflows/ci.yml`, job `android`) builds, tests and lints the debug
APK only.

## Receiving

The Receive tab turns the Go receiver on (`ReceiveService`, a
`connectedDevice` foreground service with an ongoing notification; no daily
time limit, unlike `dataSync`). It listens on 29005 (a free port when that is
taken), registers `<name>._rocket._tcp` with NSD (TXT `fp`, `ver`, `os`,
`proto=1`) and announces on the UDP beacon. Only `GET /api/hello`,
`POST /api/offer` and `POST /api/recv` are served. Offers show as a
high-priority notification (Accept / Decline) and in the app (Accept /
Always accept from this device / Decline); while the app is on screen the
notification is posted silently, so it does not pop up over the sheet. Files go to `Download/rocket`
through MediaStore on Android 10+, or through the file system with
`WRITE_EXTERNAL_STORAGE` on Android 8 and 9. Trusted senders are listed in
Settings, with revoke and the auto-accept switch. Receiving stays on until
it is turned off; the choice is remembered and restored when the app opens.

## Device test (emulator)

Checked on an API 36 x86_64 emulator (AVD `rocket_api36`) against desktop
rocket on the host, with SHA-256 compares of every file:

- desktop -> phone: turn receiving on in the Receive tab, then
  `adb forward tcp:29405 tcp:29005` and send to `127.0.0.1:29405` (offers
  then show "from 127.0.0.1").
- phone -> desktop: the host is `10.0.2.2` from the emulator. Beacon
  broadcasts do not cross the emulator NAT; to list desktop hosts in the
  Send tab, forward a host UDP port to the phone's beacon port
  (`adb emu redir add udp:29450:29006`) and send each host's beacon JSON
  (`{"name","fp","port","ver","os"}`, as in docs/protocol.md) to
  `127.0.0.1:29450`; the phone then sees them at `10.0.2.2:<port>`.

Covered: single file and folder both ways, the share sheet, decline,
"Always accept" then auto-accept, a second `photos` saved as
`photos (1)`, cancel on either side (partial files removed, including the
pending MediaStore entry), the offer notification's Accept action, and
200% font size. The site screenshots `site/assets/shots/android-*.png` come
from this setup (720x1600 at 280 dpi, SystemUI demo mode).

## Resources

- Launcher, monochrome and notification icons are drawn by
  `go run ./tools/icongen` (from the repository root).
- `res/font/` holds Inter and JetBrains Mono (SIL OFL 1.1), the Latin subsets
  in `web/vendor/fonts` converted from WOFF2 to TTF.
- Icons in the UI are Lucide (ISC), the same path data as `web/icons.svg`.
- `assets/licenses/THIRD_PARTY_LICENSES.txt` (shown in Settings > About >
  Open-source licences) is written by `go run ./tools/licenses` from the
  repository root: the Go toolchain, the Go modules linked into
  `mobile/rocketcore` for Android, the gomobile runtime, the fonts, Lucide and
  AndroidX/Compose/Kotlin. `go run ./tools/licenses -check` (run by CI and the
  release scripts) fails when it is out of date.
