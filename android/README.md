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

## Resources

- Launcher, monochrome and notification icons are drawn by
  `go run ./tools/icongen` (from the repository root).
- `res/font/` holds Inter and JetBrains Mono (SIL OFL 1.1), the Latin subsets
  in `web/vendor/fonts` converted from WOFF2 to TTF.
- Icons in the UI are Lucide (ISC), the same path data as `web/icons.svg`.
