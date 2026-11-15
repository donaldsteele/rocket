# Changelog

All notable changes to rocket are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and rocket uses
[semantic versioning](https://semver.org/).

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
- **Release zips** for Windows, macOS and Linux (amd64 and arm64) with
  `SHA256SUMS`, third-party licenses, and an icon and version info on
  `rocket.exe`.

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
- Traffic is plain HTTP; see `docs/security.md`.

[1.0.0]: https://github.com/donaldsteele/rocket/releases/tag/v1.0.0
