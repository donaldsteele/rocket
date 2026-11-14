# rocket security notes

rocket moves files between computers on a local network. This page says what
it protects against, what it does not, and why. The wire details are in
[protocol.md](protocol.md).

## Summary

- Anyone on your network can **offer** you files. Nothing is saved until you
  accept, unless you told rocket to always accept from that sender.
- A sender is identified by an ed25519 key, not by its name or IP address.
  "Always accept from this host" pins that key. Another computer cannot pass
  itself off as a trusted sender without its private key.
- The full UI and every control API answer **only on this computer**. Other
  computers see the peer protocol and the drop page, nothing else.
- Traffic is **plain HTTP**. Someone who can watch your network can see what
  is sent and could change it in transit. Use rocket on networks you trust.
- AI agents connected over MCP can list peers and send files. They can never
  accept a transfer, trust or revoke a sender, change settings or quit.

## What rocket listens on

| port            | what                                    | who can reach it |
|-----------------|-----------------------------------------|------------------|
| TCP 29005       | HTTP: UI, APIs, drop page               | every host on the network (but see the two surfaces below) |
| UDP 29006       | discovery beacon                        | every host on the subnet |
| UDP 5353        | mDNS (`_rocket._tcp.local`)             | every host on the subnet (`--no-mdns` turns it off) |

### Two surfaces on one port

Every HTTP route is listed once in a table (`internal/httpapi/server.go`) as
either **LAN** or **local**:

- **LAN** (any host): `GET /api/hello`, `POST /api/offer`, `POST /api/recv`,
  the drop page and `POST /drop/offer`, `POST /drop/upload`, plus the static
  files the drop page needs.
- **Local** (this computer only): the full UI, `GET /api/events`, peers,
  incoming offers, transfers, history, settings, trusted senders, accept, send,
  relay, cancel and quit.

A local route answers **403** unless all of these hold:

1. The connection comes from a loopback address.
2. The `Host` header is `127.0.0.1:<port>`, `localhost:<port>` or
   `[::1]:<port>`. This stops DNS rebinding: a web page on some other site that
   points its own host name at 127.0.0.1 still sends its own name in `Host`.
3. For anything but GET and HEAD, the header `X-Rocket-CSRF: 1` is present. A
   web page on another origin cannot add a custom header without a CORS
   preflight, and rocket never approves one.

A test calls every route from a non-loopback address and checks for 403, so a
new route cannot be added without picking a surface.

## Threats and what stops them

### Someone on the network sends you files you did not want

Every offer needs a decision on the receiving computer. An offer waits at most
2 minutes, then counts as timed out. At most 16 offers can wait at once, and at
most 4 from one IP address, so a flood of offers cannot pile up prompts or
memory. An offer body is capped at 1 MiB, a transfer at 10,000 entries and
1 TiB.

The prompt shows the sender's name, fingerprint, address, file count, total
size and the first few names. Names come from the other computer: treat them
as claims, and compare the fingerprint when it matters.

### Someone pretends to be a sender you trust (spoofing)

Every peer offer is signed with the sender's ed25519 key. The receiver checks:

- the fingerprint is derived from the public key in the offer,
- the offer names this receiver's fingerprint (so an offer meant for another
  host cannot be redirected here),
- the signature covers the receiver, sender, timestamp, nonce and a hash of the
  full file list,
- the key matches the key pinned when you chose "Always accept from this host".

A trusted fingerprint presented with a different key gets a normal prompt.
Changing the name or IP address of a host does not change its fingerprint.

### Someone replays a captured offer

Offers carry a timestamp (accepted within 5 minutes of the receiver's clock)
and a random nonce. The receiver remembers the last 4096 (sender, nonce) pairs
and refuses a repeat; an offer older than the window is refused anyway. Upload tokens are one-shot, expire after 10 minutes and are
only accepted from the IP address that made the offer.

Keep the clocks of your computers roughly right (within 5 minutes); network
time does this for you on most systems.

### A file list that writes outside the download folder (path traversal, zip-slip)

The file list is checked before anything is asked or written: paths must be
relative and clean, with no `..`, no absolute, drive or UNC paths, no
backslashes, colons, NUL or control characters, no duplicates (case-blind) and
no file that is also a folder. The tar stream must match the list exactly;
symlinks, hard links and devices are refused. Each top-level item gets a fresh
name in the download folder, everything below it is joined and re-checked
under that folder, and Windows reserved names (CON, NUL, COM1, ...) and
trailing dots and spaces are made safe. Files are written as `.rocket-part` and
renamed when complete; a failed or cancelled transfer removes what it wrote.

### A web page in your browser tries to drive rocket

See the two surfaces above: loopback plus the Host allowlist plus the CSRF
header. The UI also sends `X-Frame-Options: DENY`, `X-Content-Type-Options:
nosniff` and `Referrer-Policy: no-referrer`. The page loads nothing from the
internet (fonts, scripts and icons are embedded).

### A guest with a browser (drop page)

A guest can offer files, which you accept or decline like any other offer.
After your first accept the guest's browser gets a `rocket_drop` cookie
(HttpOnly, SameSite=Strict, 30 days); while **auto-accept trusted senders** is
on, that browser can drop without asking again. rocket stores only a SHA-256
hash of each cookie token. To stop remembered browsers, turn auto-accept off
in Settings.

The cookie is sent over plain HTTP, so someone watching the network could copy
it and drop files as that browser. They still could not read anything from
your computer.

### An AI agent goes too far

The MCP server has exactly four tools: `list_peers`, `send_file`,
`get_status` and `list_history`. A test pins that set and checks no tool can
accept, trust, revoke, change settings or quit. `send_file` sends real files
from paths on this computer, so only connect agents you trust with your files.
Names of peers and files come from other computers: agents should treat them
as data, not instructions. See [mcp.md](mcp.md).

## Not protected: plain HTTP

rocket does not encrypt traffic. Anyone who can see the packets (shared Wi-Fi
without client isolation, a compromised router, a mirrored switch port) can:

- read the files being sent and the file names,
- see which hosts send to which,
- change a file in transit: the signature covers the file list and sizes, not
  the file contents,
- copy a drop cookie.

They cannot make your computer accept an offer from a trusted sender without
that sender's private key, and they cannot reach the local control API.

HTTPS with a local certificate authority was considered and left out of 1.0:
it adds a certificate step on every host and phone, and the main risk on a home
or office network is an unwanted sender, which the signed, pinned offers
address. If you need confidentiality, send over a network you control or
encrypt the files first.

## Other limits

- **No resume.** An interrupted transfer starts again from the beginning.
- **Unsigned binaries.** Release zips are not code-signed or notarized. Check
  them against `SHA256SUMS` from the release page.
- **Firewall.** rocket needs TCP 29005 and UDP 29006 (and UDP 5353 for mDNS)
  open on private networks. Do not open them on public networks.
- **Your data folder** (`node.key`, `trusted.json`, `drop_tokens.json`,
  `settings.json`, `history.jsonl`) is protected only by your user account's
  file permissions. Anyone who can read `node.key` can act as this host.

## Reporting a problem

Please open an issue at https://github.com/donaldsteele/rocket/issues. For
something sensitive, say so in the issue without the details and ask for a
private contact.
