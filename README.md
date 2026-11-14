# rocket

rocket is a single-binary file transfer tool for a local network. Run it on two
or more computers on the same network and send files between them directly, with
a quick approval on the receiving end. No cloud, no account, no internet.

It is a companion to [netdiag](https://github.com/donaldsteele/netdiag) and
[bubbles](https://github.com/donaldsteele/bubbles) and shares their shape: one
binary for Windows, macOS and Linux, a local browser UI in a warm theme, and an
MCP server so an AI agent can help.

## What it does

- **Peer to peer:** every computer runs rocket. It finds the others on the
  network automatically and lists them. Pick a peer, choose files, send.
- **Drop page:** a phone or guest with no binary opens the rocket page in a
  browser and uploads straight to a computer on the network.
- **Trust on first use:** the first transfer from a new sender asks the receiver
  to approve it, then remembers that sender. Browsers are remembered per device.
- **Received files** land in your **Downloads/rocket** folder.
- **AI agent access (MCP):** Claude or another agent can list peers and send a
  file. It can never accept an incoming transfer — that stays with you.

## Quick start

1. Download the zip for your system, check it against `SHA256SUMS`, and unzip it.
   See `HOW-TO-OPEN.txt` for the SmartScreen (Windows) and Gatekeeper (macOS)
   steps for unsigned programs.
2. Run `rocket` on each computer. It prints the addresses to open and opens the
   local page in your browser.
3. On the sending computer, the other computers appear under **Peers**. Click
   **Send file…**, choose a file.
4. On the receiving computer, approve the transfer when it is offered. Tick
   **Always accept from this host** to skip the prompt next time.

## Command line

```
rocket            start the web UI and announce this host (default)
rocket ui         same, with --no-browser to skip opening a window
rocket send <peer> <file>   send a file to a peer by name or fingerprint
rocket recv       run headless: announce and accept, no browser
rocket mcp        serve the MCP interface for an AI agent over stdio
rocket version    print the version
```

## Connect Claude (MCP)

```
claude mcp add rocket -- "C:\path\to\rocket.exe" mcp      # Windows
claude mcp add rocket -- /path/to/rocket mcp              # macOS / Linux
```

Claude can then `list_peers` and `send_file`. It cannot accept an incoming
transfer, trust or revoke a sender, change settings, or quit rocket.

## How it works

- **Identity:** on first run rocket makes an ed25519 keypair in its data
  directory. The short fingerprint of the public key is its stable ID, so trust
  survives an IP or hostname change.
- **Discovery:** a small UDP broadcast beacon on port 29006 announces each host
  (name, fingerprint, HTTP port, version, OS); every host keeps a live peer list
  and expires silent peers. (mDNS can replace this later.)
- **Transfer:** a two-phase push on the HTTP port (29005): the sender offers
  metadata, the receiver approves (trust-on-first-use), then the sender streams
  the bytes straight to disk. Nothing is held whole in memory.

## Status

This is an early scaffold. The transfer wire, discovery, trust store, web UI and
MCP server work. Not yet done: mDNS, HTTPS, folder sends, resume, history, the
desktop tray app, and packaging/signing.

## Security

rocket binds the local network interface, so any host on your subnet can reach
it. Trust-on-first-use gates who may send you a file, but traffic is currently
plain HTTP. Use it on networks you trust. HTTPS with a local CA (as bubbles
does) is planned.

## License

Apache-2.0. See `LICENSE`.
