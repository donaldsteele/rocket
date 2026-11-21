# Changelog

All notable changes to rocket are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and rocket uses
[semantic versioning](https://semver.org/).

## [Unreleased]

### Changed

- The tray icon shows an open pairing window: an amber badge, and a tooltip
  that says pairing is open and how long is left. The red badge for waiting
  offers still wins, with the pairing note added to its tooltip.
- `tools/icongen` documents running with the release toolchain (Go 1.26.9):
  Go 1.27 draws the same pixels but compresses the PNGs differently.

## [1.2.0] - 2026-11-21

Send to an address, trust from the command line, and pairing. Works with
rocket 1.1 (same protocol); rocket 1.0 still needs updating.

### Added

- **Send to an address** in the desktop page: under the peer list, type a
  host or IP address (port 29005 if left out) and send files or a folder to a
  computer or phone that discovery does not find, such as one on another
  subnet or a Tailscale tailnet. The page remembers the last five addresses.
- `rocket send` takes a bare IP address (`rocket send 100.101.102.103 ./photos`),
  with port 29005.
- MCP `send_file` takes a private IP address (RFC 1918, Tailscale's
  100.64.0.0/10, IPv6 ULA and link-local) as `peer`. Host names and public
  addresses are refused, so an agent cannot be talked into sending files to a
  host on the internet.
- **Trust senders from the command line**, for headless receivers and
  setting devices up ahead of time: `rocket trust list`,
  `rocket trust add <public-key|fingerprint> [--name NAME]`,
  `rocket trust remove <fingerprint>`, and `--trust` on `rocket recv` and
  `rocket ui`. `rocket id` prints this device's name, fingerprint and public
  key with the command that trusts it. A sender added by public key is pinned
  to that key; one added by fingerprint is accepted when its first verified
  offer's key matches, and that key is pinned from then on. A rocket running
  on the same data folder picks up the change without a restart (the trust
  store now rereads `trusted.json` when it changes and before every write,
  so the UI and the command line do not overwrite each other).
- **Pair new devices**: a pairing window (5 minutes by default, at most an
  hour) in which every rocket device that sends is accepted without asking
  and trusted, for setting up your own devices quickly. In the desktop page
  (Receive card, with countdown, Stop and the devices paired), the tray menu,
  the Android app (Receive tab), and the command line: `rocket pair [5m|stop|status]`
  for a running rocket, and `--pair 5m` on `rocket ui` and `rocket recv`.
  Local API `GET/POST /api/pair` and the events `pairing` and `paired`.
  Browser drops are never paired, and MCP agents cannot open a window.
- README and site: using rocket over Tailscale, and use cases.

### Fixed

- A file that cannot be read on the sending side is reported as a local read
  error, instead of as the receiver giving up (410).
- A cancel notice that arrived while a broken upload was ending could leave
  its kept part on the receiver for 24 hours; it is now discarded.

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

[1.2.0]: https://github.com/donaldsteele/rocket/releases/tag/v1.2.0
[1.1.0]: https://github.com/donaldsteele/rocket/releases/tag/v1.1.0
[1.0.0]: https://github.com/donaldsteele/rocket/releases/tag/v1.0.0
