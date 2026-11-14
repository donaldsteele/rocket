# rocket

rocket is a single-binary file transfer tool for a local network. Run it on two
or more computers on the same network and send files and folders between them
directly, with a quick approval on the receiving end. No cloud, no account, no
internet.

It is a companion to [netdiag](https://github.com/donaldsteele/netdiag) and
[bubbles](https://github.com/donaldsteele/bubbles) and shares their shape: one
binary for Windows, macOS and Linux, a local browser UI in a warm theme, and an
MCP server so an AI agent can help.

## What it does

- **Peer to peer:** every computer runs rocket. It finds the others on the
  network automatically (UDP beacon and mDNS) and lists them. Pick a peer,
  choose files or a folder, send.
- **Files and folders:** many files or a whole folder go as one transfer with
  one approval, with per-file progress. Either side can cancel.
- **Drop page:** a phone or guest with no rocket opens the rocket address in a
  browser and uploads straight to that computer.
- **Trust on first use:** the first transfer from a new sender asks the receiver
  to approve it. Tick **Always accept from this host** to remember that
  sender's key. Every offer is signed, so another computer cannot pretend to be
  a sender you trust.
- **History and settings:** see past transfers, change the device name and
  download folder, and revoke trusted senders.
- **Received files** land in your **Downloads/rocket** folder.
- **Tray icon** (Windows and Linux) shows when someone is waiting for an answer.
- **AI agent access (MCP):** Claude or another agent can list peers and send
  files. It can never accept an incoming transfer, that stays with you.

## Quick start

1. Download the zip for your system from the
   [releases page](https://github.com/donaldsteele/rocket/releases), check it
   against `SHA256SUMS`, and unzip it.

   | system                         | zip                        |
   |--------------------------------|----------------------------|
   | Windows PC (most laptops)      | `rocket-windows-amd64.zip` |
   | Windows on ARM                 | `rocket-windows-arm64.zip` |
   | Mac with Apple chip            | `rocket-darwin-arm64.zip`  |
   | Mac with Intel chip            | `rocket-darwin-amd64.zip`  |
   | Linux PC                       | `rocket-linux-amd64.zip`   |
   | Linux on ARM (Raspberry Pi 4/5)| `rocket-linux-arm64.zip`   |

   See `HOW-TO-OPEN.txt` for the SmartScreen (Windows) and Gatekeeper (macOS)
   steps for unsigned programs, and for the firewall prompt.
2. Run `rocket` on each computer. It prints the addresses to open and opens the
   local page in your browser.
3. On the sending computer, the other computers appear under **Computers on
   your network**. Pick one, then **Send files** or **Send folder**.
4. On the receiving computer, **Accept** or **Decline** the offer. Tick
   **Always accept from this host** to skip the prompt next time.

To receive from a phone, open the **drop page** address rocket prints (for
example `http://192.168.1.20:29005/`) in the phone's browser and choose files.

## Command line

```
rocket                     start the web UI and announce this host (default)
rocket ui                  same; --no-browser skips opening a window
rocket send <peer|host:port> <path>...
                           send files and folders to a peer by name,
                           fingerprint prefix, or address
rocket recv                run headless: announce and receive, no browser
rocket mcp                 serve the MCP interface for an AI agent over stdio
rocket version             print the version
```

Flags for `ui`, `send`, `recv` and `mcp`:

| flag             | default                  | environment          |
|------------------|--------------------------|----------------------|
| `--port`         | 29005 (TCP)              | `ROCKET_PORT`        |
| `--beacon-port`  | 29006 (UDP)              | `ROCKET_BEACON_PORT` |
| `--data-dir`     | see below                | `ROCKET_DATA_DIR`    |
| `--download-dir` | `Downloads/rocket`       | `ROCKET_DOWNLOAD_DIR`|
| `--name`         | the computer's host name | `ROCKET_NAME`        |
| `--no-mdns`      | mDNS on                  |                      |

A flag wins over the environment, which wins over Settings, which wins over the
default. Every host on a network must use the same beacon port to see each
other by beacon.

## Where the data is kept

The data folder is `rocket` inside your user configuration folder
(`%AppData%\rocket` on Windows, `~/Library/Application Support/rocket` on
macOS, `~/.config/rocket` on Linux), unless `--data-dir` says otherwise:

- `node.key`: this host's private key. Keep it private.
- `trusted.json`: senders you chose to always accept (fingerprint, name, key).
- `drop_tokens.json`: hashes of remembered drop-page browsers.
- `settings.json`: device name, download folder, auto-accept.
- `history.jsonl`: the last 1000 transfers.

## Tray app

`rocket-tray` is an optional icon for the system tray (Windows notification
area, Linux StatusNotifier tray). Put it next to `rocket` (the zips ship them
together) and run it. Its menu shows:

- whether rocket is running and how many peers it sees,
- how many incoming offers are waiting (the icon gets a red badge; click the
  line to open rocket and answer),
- **Open rocket**, **Start rocket** (runs `rocket ui --no-browser` in the
  background when it is not running), **Quit rocket**, and **Exit tray**
  (closes only the icon; rocket keeps running).

It checks every 2 seconds and talks only to rocket's local API on
`http://127.0.0.1:29005`. Use `--port` (or `ROCKET_PORT`) when rocket runs on
another port, and `rocket-tray --status` to print the status once.

Release zips include the tray for Windows and Linux (amd64 and arm64). On Linux
the desktop needs a StatusNotifier tray (KDE, or GNOME with the AppIndicator
extension). macOS tray binaries are not shipped, because the macOS tray needs
cgo; build it from source on a Mac:

```
cd tray
go build -o rocket-tray .
```

## Connect Claude (MCP)

```
claude mcp add rocket -- "C:\path\to\rocket.exe" mcp      # Windows
claude mcp add rocket -- /path/to/rocket mcp              # macOS / Linux
```

Claude can then use `list_peers`, `send_file`, `get_status` and
`list_history`. It cannot accept an incoming transfer, trust or revoke a
sender, change settings, or quit rocket. See [docs/mcp.md](docs/mcp.md).

## Firewall

rocket needs these open on **private** networks:

- TCP **29005**: transfers, the drop page (and the UI, which answers only on
  this computer),
- UDP **29006**: the discovery beacon,
- UDP **5353**: mDNS (optional; `--no-mdns` turns it off).

Windows asks the first time rocket runs: allow it on Private networks. On
Linux with `ufw`:

```
sudo ufw allow 29005/tcp
sudo ufw allow 29006/udp
sudo ufw allow 5353/udp
```

## How it works

- **Identity:** on first run rocket makes an ed25519 keypair in its data
  folder. The short fingerprint of the public key is its stable ID, so trust
  survives an IP or host name change.
- **Discovery:** a UDP broadcast beacon on port 29006 and mDNS
  (`_rocket._tcp.local`) announce each host; both feed one peer list keyed by
  fingerprint, and silent peers expire.
- **Transfer:** a two-phase push on the HTTP port: the sender sends a signed
  offer listing every file, the receiver approves (trust on first use), then
  the sender streams the bytes (one file raw, several as a tar stream) straight
  to disk. Nothing is held whole in memory. See
  [docs/protocol.md](docs/protocol.md).

## Security

rocket listens on your local network, so any host on your subnet can **offer**
you files; nothing is saved until you accept. The full UI and every control
API answer only on this computer. Offers are signed and trusted senders are
pinned by key.

Traffic is **plain HTTP**: someone who can watch your network can see what is
sent. Use rocket on networks you trust. The full threat model is in
[docs/security.md](docs/security.md).

## Status

Version 1.0. Not included: encryption in transit (HTTPS), resuming an
interrupted transfer, code signing, macOS tray binaries, and transfers over
the internet. See [CHANGELOG.md](CHANGELOG.md).

## Building from source

Go 1.26 or later.

```
go build ./cmd/rocket                         # this platform
VERSION=1.0.0 bash scripts/build.sh           # all release zips into dist/
pwsh scripts/build.ps1 -Version 1.0.0         # same, from PowerShell
go run ./tools/licenses                       # refresh THIRD_PARTY_LICENSES.txt
```

The build script cross-compiles rocket for six targets and the tray for four,
adds the icon and version info to `rocket.exe`, and writes `dist/SHA256SUMS`.

## License

Apache-2.0. See `LICENSE` and `NOTICE`. Third-party licenses are in
`THIRD_PARTY_LICENSES.txt`.
