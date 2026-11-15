# Changelog

All notable changes to rocket are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and rocket uses
[semantic versioning](https://semver.org/).

## [1.1.0] - 2026-11-15

Encrypted transfers and resume.

### Breaking

- **Protocol 2. rocket 1.1 does not talk to rocket 1.0**, in either
  direction, and there is no unencrypted fallback. Update every computer and
  phone. Sending to a 1.0 device fails with "<name> runs an older rocket
  (protocol 1); update it to 1.1 or later", and a 1.0 sender's offer is
  answered 426 "this device runs rocket 1.1 or later; update the sender".
  Peer lists show 1.0 devices as **Needs update** and they cannot be picked.
  Settings, trusted senders and history carry over when 1.1 is installed over
  1.0.

### Added

- **Encrypted transfers** between rocket apps (desktop and Android). Each
  offer carries a fresh X25519 key, the receiver answers with its own in a
  reply signed with its ed25519 key, and the file contents travel as a
  ChaCha20-Poly1305 stream in 64 KiB frames under a key derived (HKDF-SHA256)
  for that one transfer. A changed, reordered or extended stream fails
  and the transfer is thrown away; one cut short is kept for a resume. Only Go's standard library and
  `golang.org/x/crypto` are used. See `docs/protocol.md`.
- **Signed replies.** Every answer to an offer, accepted or not, is signed by
  the receiver and bound to the offer it answers.
- **Resume.** When an accepted transfer breaks for a network reason, the
  receiver keeps what arrived for 24 hours, the sender retries by itself after
  2, 5, 10, 20 and 30 seconds (**Reconnecting n/5**), and only the missing
  bytes are sent, without a second prompt. After the last try the send is
  **Interrupted**, with **Resume** and **Discard**; the receiver lists what it
  kept with **Discard**. Works for every send between rocket apps (files
  picked in the desktop page, `rocket send`, MCP and the Android app) and
  for receiving on both desktop and Android. Files picked in the desktop
  page stay in the browser: the page makes the retries itself, and after a
  reload Resume asks for the same files or folder again and refuses
  different ones. Cancel or decline never keeps anything. Retries and
  Resume go only to the receiver that accepted the transfer, also for a
  send to a bare address.
- **Status in the UI**: an **Encrypted** / **Not encrypted** badge on offers,
  transfers and history, **Needs update** peers, **Reconnecting n/5**, and
  **Interrupted** in History, on the desktop and on Android.
- `rocket send` prints its reconnect attempts, and running the same command
  again continues a send that gave up.
- MCP: `send_file` reports `encrypted`, retries and `interrupted`;
  `get_status` reports the protocol, transfers in progress, interrupted sends
  and peers that need an update; `list_peers` reports `proto` and
  `needsUpdate`.
- Discovery announces the protocol (beacon `"proto":2`, mDNS TXT
  `proto=2`); a missing value means protocol 1.
- Android: persistable read permission for shared files where the provider
  allows it, so Resume works after the app was closed; otherwise Resume asks
  for the files again. versionCode 2.

### Changed

- The transfer body is the files' bytes in manifest order behind a 16-byte
  start header, instead of a tar stream (the signed file list already fixes
  names, order and sizes).
- History entries record `encrypted`, and a new result `interrupted`.
- The data folder gains `resume/` and `outgoing/` for kept and
  interrupted transfers.

### Fixed

- Transfers that were reported "cancelled by the receiver" when built with
  Go 1.27 (connection reuse after a completed transfer). CI now tests Go 1.26
  and 1.27.

### Security

- The browser drop page stays plain HTTP and is **not encrypted** (browsers
  only allow Web Crypto on HTTPS pages); offers and history mark it **Not
  encrypted**, and it does not resume.
- File names, sizes, folder layout and device names are still visible on the
  network: offers are signed, not encrypted. See `docs/security.md`.
- A transfer resumes only for an offer signed by the same sender key for the
  same file list, and only at the point the receiver offered in its signed
  reply; the sender checks its copy of the kept part first. For files
  picked in the desktop page, rocket takes that point from the signed reply
  and hashes what the page uploads; the page cannot choose it.

## [1.0.0] - 2026-11-14

The first release.

### Added

- **Peer-to-peer transfers on the local network.** Two-phase push on one HTTP
  port (29005): offer, approve, stream straight to disk. Received files land in
  `Downloads/rocket`.
- **Multi-file and folder sends** in one offer with one approval prompt, sent
  as a tar stream, with per-file progress. Empty folders, unicode names and
  0-byte files are kept.
- **Signed offers (protocol v1).** Every offer is ed25519-signed and bound to
  the receiver's fingerprint, a timestamp and a one-time nonce. Trust on first
  use pins the sender's full public key. See `docs/protocol.md`.
- **Transfer manager.** Every transfer has an ID; cancel from either end
  removes partial files.
- **History** of finished transfers (`history.jsonl`, last 1000), with a
  History view and the MCP tool `list_history`.
- **Settings**: device name, download folder, auto-accept trusted senders, and
  the list of trusted senders with revoke.
- **Discovery** by UDP beacon (29006) and mDNS (`_rocket._tcp.local`), merged
  by fingerprint. `--no-mdns` turns mDNS off.
- **Browser drop page** for phones and guests without rocket: choose files or
  drag and drop, per-file progress. Remembered browsers use a server-side
  hashed cookie token.
- **Web UI** with a sidebar (Send, Transfers, History, Settings), light and
  dark themes, keyboard use, screen reader announcements, and a layout that
  works down to 360 px.
- **Tray app** (`rocket-tray`) for Windows and Linux: status, pending offer
  badge, open, start and quit rocket.
- **MCP server** (`rocket mcp`) with `list_peers`, `send_file`, `get_status`
  and `list_history`. Agents can never accept, trust, revoke, change settings
  or quit.
- **Command line**: `rocket send <peer|host:port> <path>...`, `rocket recv`,
  and the flags `--port`, `--beacon-port`, `--data-dir`, `--download-dir`,
  `--name`, `--no-mdns` (with `ROCKET_*` environment overrides).
- **Android app** (`rocket-android.apk`, Android 8.0 and later), built on the
  same Go protocol code as the desktop (`gomobile bind`):
  - Send files or a folder to computers and phones it finds on the network
    (UDP beacon and Android NSD), or to an address; share to rocket from any
    app.
  - Receive with the same signed offers and trust on first use: a
    notification with Accept and Decline, an in-app sheet with **Always
    accept from this device**, files saved to `Download/rocket`. Receiving
    runs as a foreground service only while it is turned on, and the phone
    serves only the peer protocol.
  - Transfers with progress and cancel, history, device name, trusted senders
    with revoke, auto-accept, light and dark themes, TalkBack labels and font
    scaling to 200%.
- **Release zips** for Windows, macOS and Linux (amd64 and arm64), and the
  signed universal `rocket-android.apk`, with `SHA256SUMS`, third-party
  licenses, and an icon and version info on `rocket.exe`.

### Security

- The full UI and control APIs answer only on this computer (loopback address,
  Host header allowlist, `X-Rocket-CSRF` header on every change). Other hosts
  reach only the peer protocol and the drop page.
- Path traversal, symlinks, devices and Windows reserved names are refused or
  made safe on receive; files are written as `.rocket-part` and renamed when
  complete.
- Limits on offer size, entries, total bytes and pending offers; offers are
  rate-limited per IP, offer bodies must arrive within 30 seconds, and one LAN
  address can hold at most 32 connections.
- The replay cache never forgets a nonce that is still inside the freshness
  window; a flood gets "busy" (503) instead.
- A full fingerprint only ever matches that exact peer, and a name or prefix
  that matches several peers is refused, so a host cannot capture sends by
  announcing another peer's fingerprint as its name.
- Quitting cancels transfers in flight cleanly (partials removed, history
  written).
- The Android app keeps its key, trusted senders and history out of backups
  and device-to-device transfers. Its release APK is signed with a key that
  never enters the repository or CI; CI builds only a debug APK.
- Traffic is plain HTTP; see `docs/security.md`.

[1.1.0]: https://github.com/donaldsteele/rocket/releases/tag/v1.1.0
[1.0.0]: https://github.com/donaldsteele/rocket/releases/tag/v1.0.0
