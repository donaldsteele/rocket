# Plan: rocket Android app (ships with v1.0)

## 1. Context & Scope

rocket v1.0 desktop (T1–T9 in `roadmap/v1.0.md`) is built. The user wants an Android app that
can **send and receive** with desktop rocket and other phones, released **together with v1.0**
(the local `v1.0.0` tag from the first workflow is deleted and re-created on the final commit).
Also merged before release: the GitHub Pages site (`site/`, branch `site`).

In scope: Android app (phones, Android 8.0 / API 26+), share-sheet sending, file/folder picking,
peer discovery, send with progress/cancel, receive with accept prompt (TOFU), history, settings,
trusted senders, signed release APK attached to the GitHub release, docs and site section, CI
build. Out of scope: Play Store, iOS, tablets-specific layouts, Android drop page, background
receive while the receiver toggle is off, Wear/TV.

## 2. Locked Decisions (Non-Negotiable)

1. **Kotlin UI + Go core.** UI, Android integration (share sheet, SAF, NsdManager, services,
   notifications, MediaStore) in Kotlin with Jetpack Compose + Material 3. Protocol v1 (signing,
   verification, manifests, tar streaming, limits, path safety, HTTP send/receive, trust store,
   history) comes from rocket's Go code through `gomobile bind` — one implementation of the
   wire. No Kotlin re-implementation of protocol logic.
2. **Same trust model as desktop.** Signed offers, pinned keys, TOFU accept prompt on the phone
   (notification + in-app sheet), Android exposes only the LAN surface (`/api/hello`,
   `/api/offer`, `/api/recv`) — no control API, no drop page, no UI over HTTP. Plain HTTP on the
   LAN (cleartext allowed via network security config), documented.
3. **Release integrity.** Release APK signed with the key at
   `%USERPROFILE%\.rocket-signing\` (`keystore.properties`, read via env
   `ROCKET_KEYSTORE_PROPS` or that default path). The keystore and its password never enter the
   repo, logs or CI. CI builds debug only. Generated AAR and build outputs are gitignored.

## 3. System Architecture & Module Layout

- **Go**
  - `internal/receiver/` (new): the peer-facing receive path extracted from `internal/httpapi`
    — hello/offer/recv handlers, verification, limits, pending offers, tokens, manifest checks,
    safe tar/single-file writing — behind two interfaces: `Decider` (ask the user; answer later
    by offer ID) and `Sink` (create/commit/abort an output file for a validated relative path).
    Desktop: `httpapi` uses it with a filesystem sink (existing behaviour, `.rocket-part` +
    rename, unique names). Behaviour and tests of desktop must not change.
  - `mobile/rocketcore/` (new, gomobile-bindable, Java package
    `io.github.donaldsteele.rocket.core`): `Node` (identity, trust, history in the app's files
    dir, name), beacon discovery start/stop with a `PeerListener`, `Send(addr, fp, Source,
    ProgressListener)` returning a transfer ID, `Cancel(id)`, `StartReceiver(port, Decider,
    Sink, EventListener)` / `StopReceiver()`, `Answer(offerID, accept, trust)`, JSON getters
    for history/trusted, `Revoke(fp)`. Only gomobile-safe types (string, bool, int/int64,
    []byte, interfaces, simple structs). File content crosses as **file descriptors** (Kotlin
    `ParcelFileDescriptor.detachFd()`), Go wraps with `os.NewFile`; a chunked-read fallback for
    providers without fds. Events are JSON strings in the same shapes as desktop SSE events.
  - Go interop tests: mobile `Node` sending to a desktop `httpapi` server and desktop
    `transfer` sending to a mobile receiver with a temp-dir sink (single file + folder,
    decline, cancel, trusted auto-accept, forged offer rejected).
  - `scripts/build-android.sh` / `.ps1`: `gomobile bind -target=android/arm64,android/arm,android/amd64
    -androidapi 26 -javapkg io.github.donaldsteele.rocket.core -o android/app/libs/rocketcore.aar
    ./mobile/rocketcore`, then Gradle.
- **Android** (`android/`, Gradle Kotlin DSL, version catalog, current stable AGP/Kotlin/Compose
  BOM, compileSdk/targetSdk 36, minSdk 26, applicationId `io.github.donaldsteele.rocket`,
  versionName `1.0.0`)
  - Screens: **Send** (peers with status shape + word; pick files or a folder via SAF), **Share**
    entry (`ACTION_SEND`/`ACTION_SEND_MULTIPLE`, `*/*`) -> choose peer -> send, **Transfers**
    (progress, cancel), **Receive** (toggle; "visible as <name> · <fp>"; incoming offer sheet
    with Accept / Decline / Always accept from this device), **History**, **Settings** (device
    name, trusted senders with revoke, about/version, security note).
  - Discovery: NsdManager browse `_rocket._tcp` + Go beacon listener (hold
    `WifiManager.MulticastLock` while discovering), merged by fingerprint.
  - Receive: foreground service runs the Go receiver on 29005 (fallback to a free port if taken,
    advertised accordingly), registers NSD `<name>._rocket._tcp` with TXT `fp`, `ver`, `os`,
    `proto=1`, and the Go beacon announce. Offers raise a high-priority notification with
    Accept/Decline actions and the in-app sheet. Files go to `Download/rocket/` via MediaStore
    (API 29+) or the public Downloads dir with `WRITE_EXTERNAL_STORAGE` (maxSdk 28).
  - Long sends/receives run in a foreground service so they survive the app going to the
    background. Permissions: INTERNET, ACCESS_NETWORK_STATE, ACCESS_WIFI_STATE,
    CHANGE_WIFI_MULTICAST_STATE, FOREGROUND_SERVICE (+ the matching type), POST_NOTIFICATIONS.
  - Theme: warm tokens from `web/styles.css` (dark default, light, follows system), Inter +
    JetBrains Mono from `web/vendor/fonts` (OFL), adaptive launcher icon from `tools/icongen`.
  - Accessibility: TalkBack labels, 48dp targets, AA contrast, no colour-only status, font
    scaling to 200%.

## 4. Interfaces & Contracts

- Wire protocol: unchanged protocol v1 (`docs/protocol.md`). Android peers report `os:"android"`.
- Desktop UI shows Android peers like any other (icon may differ by `os`).
- `rocketcore` API as in §3; documented in `mobile/rocketcore/doc.go`.
- Release assets add `rocket-android.apk` (universal, signed) listed in `SHA256SUMS`.

## 5. Risk Mitigation Matrix

1. **Protocol drift between platforms** — Go core only; interop tests in `go test ./...`.
2. **Refactor breaks desktop** — extraction keeps the existing httpapi tests green unchanged;
   any test edits must be mechanical (imports), reviewed in A6.
3. **Android background/foreground-service limits** — foreground services with a declared type
   and visible notification; receiver only runs while the user has it on; tested on API 36
   emulator (and API 26 behaviour reasoned/linted).
4. **Content URIs without real files** — fd passing with chunked fallback; tests in Kotlin for
   the source adapter; sizes from `OpenableColumns.SIZE` with streaming when unknown (manifest
   needs sizes: unknown sizes are resolved by stat on the fd or rejected with a clear message).
5. **Signing key exposure/loss** — key outside repo; build reads properties path; scripts never
   echo it; README tells the maintainer to back up `%USERPROFILE%\.rocket-signing`.

## 6. Milestones

Desktop checks (`gofmt -l`, `go build ./...`, `go vet ./...`, `go test ./...`) must stay green in
every milestone. Android checks from `android/`:
`./gradlew testDebugUnitTest lintDebug assembleDebug` (Windows: `gradlew.bat`).

- **A1: Go receiver extraction + rocketcore binding** — `internal/receiver`, `mobile/rocketcore`,
  interop tests, `scripts/build-android.*` (AAR part). Verify: Go checks + `gomobile bind`
  produces `android/app/libs/rocketcore.aar` (gitignored).
- **A2: Android app — shell, discovery, send** — Gradle project, theme, icon, Send/Share/
  Transfers/History/Settings, foreground transfer service. Verify: Android checks.
- **A3: Android receive** — receiver service, NSD advertise, offer notification + sheet,
  MediaStore sink, trusted senders. Verify: Android checks + Go interop tests.
- **A4: Device test** — install emulator + one x86_64 API 36 system image if hardware
  acceleration works; install the APK; desktop -> phone (adb forward) and phone -> desktop
  (10.0.2.2) transfers with SHA-256 compare; capture screenshots (dark/light) for README/site.
  If acceleration is unavailable, record that and fall back to Go interop + a Robolectric/UI
  unit level smoke.
- **A5: Release integration + docs** — build scripts produce signed `rocket-android.apk` into
  `dist/` and `SHA256SUMS`; HOW-TO-OPEN Android sideload steps; README + `site/` Android section
  with screenshots; CHANGELOG; CI job building the debug APK; `.gitignore`.
- **A6: Review + fixes** — independent review of `internal/receiver`, `mobile/`, `android/`,
  scripts; verified fixes.
- **R: Final release (local)** — delete local `v1.0.0` tag, version 1.0.0 everywhere, full build
  (6 desktop zips + APK + SHA256SUMS + RELEASE_NOTES.md), desktop two-instance smoke test, APK
  signature check (`apksigner verify --print-certs` matches the keystore cert SHA-256
  `46:3A:4E:FF:1B:66:1D:BA:36:FB:68:8F:0E:A6:08:90:3F:17:7E:24:B8:D6:F5:59:78:26:0E:B7:89:F4:68:E4`),
  tag `v1.0.0`. No push.
- **Publish (main session)** — create repo, push main + tag, enable Pages (workflow build),
  create the GitHub release with all assets.

## 7. Immediate First Step

A1: define the `internal/receiver` `Decider`/`Sink` interfaces and move the httpapi receive path
behind them with desktop tests unchanged and green.
