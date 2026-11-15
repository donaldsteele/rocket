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
- **File contents are encrypted** between rocket apps (desktop and Android,
  rocket 1.1 and later) with keys made fresh for every transfer, and checked
  so a change in transit is caught and the transfer thrown away. File
  **names, sizes, folder layout and device names are not encrypted**, and
  the browser drop page is not encrypted at all. See
  [What the network can still see](#what-the-network-can-still-see).
- rocket 1.1 does not talk to rocket 1.0 in either direction; it says which
  side needs updating.
- AI agents connected over MCP can list peers and send files. They can never
  accept a transfer, trust or revoke a sender, change settings or quit.
- The Android app uses the same Go code for the protocol, listens only while
  **Receive files** is on, and serves only the peer protocol. See
  [Android](#android).

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
  `POST /api/recv/cancel`, the drop page and `POST /drop/offer`,
  `POST /drop/upload`, plus the static files the drop page needs.
- **Local** (this computer only): the full UI, `GET /api/events`, peers,
  incoming offers, transfers, history, settings, trusted senders, accept, send,
  relay, cancel, resumable transfers with resume and discard, and quit.

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

### Someone watches or changes a transfer on the network

Every file body between rocket apps travels encrypted and authenticated
(protocol v2, [protocol.md](protocol.md#body-key)):

- The sender puts a fresh X25519 key in its signed offer; the receiver
  answers with its own fresh X25519 key in a reply signed with its ed25519
  key. The sender checks that signature against the key of the fingerprint it
  chose to send to, so a machine in the middle cannot swap in its own key
  without the receiver's private key. The receiver knows the sender's key is
  genuine because the offer is signed.
- Both sides derive the body key with HKDF-SHA256 from the X25519 result,
  the hash of the signed offer and the transfer ID, so every transfer has its
  own key. The ephemeral private keys live only in memory for one transfer:
  someone who records a transfer and later steals `node.key` still cannot
  decrypt it.
- The body is ChaCha20-Poly1305 in frames of 64 KiB with a counter and a
  "last frame" flag in each nonce. A changed, reordered, repeated or dropped
  frame, a body cut short, or anything added after the end fails, and the
  receiver removes everything that transfer wrote.
- The crypto is Go's standard library (`crypto/ecdh`, `crypto/hkdf`,
  `crypto/ed25519`) and `golang.org/x/crypto/chacha20poly1305`; rocket
  adds no primitives of its own. Known-answer vectors computed outside rocket,
  tamper and truncation tests and a fuzz test on the reader pin the format.

Encryption protects the transfer to the device whose fingerprint you send
to. If you send to a bare `host:port` (rocket send, or a typed address), the
transfer is encrypted to whoever answers on that address; pick peers from the
list, or compare the fingerprint, when it matters. A fingerprint is 64 bits
of a SHA-256 hash: making a different key with the same fingerprint would
take about 2^64 attempts.

### Someone interferes with an interrupted transfer (resume)

When an accepted transfer breaks for a network reason, the receiver keeps
what arrived for 24 hours and the sender offers the same files again; only
the missing bytes are sent ([protocol.md](protocol.md#resume)).

- Only bytes that passed authentication are kept, so a cut connection can
  never leave altered content behind.
- A kept transfer is continued without asking only by an offer signed with
  the **same sender key** for the **same file list** (manifest hash). Anyone
  else, or other files, gets the normal prompt. The resume point travels in
  the signed reply, and the body may start only there or at the beginning,
  so nobody can make the receiver write at an offset it did not offer.
- The sender hashes the part of its own file that the receiver says it
  holds and starts over when that does not match, so a file changed in the
  meantime is not stitched together from old and new bytes.
- A cancel on either side, a decline, a body that fails authentication or
  does not match the offer never leaves anything kept. When the sender
  gives up (cancel, a failure reading its files), it sends a cancel notice
  signed with its key; a notice signed by anyone else is refused.
- Kept data is visible: the part files stay in the download folder, the UI
  lists kept transfers with a Discard button, and they are removed after
  24 hours (swept at start-up and every hour). Someone who can cut your
  connection can make rocket keep part files for up to a day, but no more
  than you agreed to receive.

### Someone pretends to be a sender you trust (spoofing)

Every peer offer is signed with the sender's ed25519 key. The receiver checks:

- the fingerprint is derived from the public key in the offer,
- the offer names this receiver's fingerprint (so an offer meant for another
  host cannot be redirected here),
- the signature covers the receiver, sender, timestamp, nonce, the sender's
  ephemeral encryption key and a hash of the full file list,
- the key matches the key pinned when you chose "Always accept from this host".

A trusted fingerprint presented with a different key gets a normal prompt.
Changing the name or IP address of a host does not change its fingerprint.
Names come from unauthenticated announcements, so a full fingerprint never
matches a name, and a name or fingerprint prefix that matches more than one
peer is refused instead of guessed. The browser UI sends by full fingerprint.

### Someone replays a captured offer

Offers carry a timestamp (accepted within 5 minutes of the receiver's clock)
and a random nonce. The receiver remembers every (sender, nonce) pair it saw
within twice that window and refuses a repeat; an offer older than the window
is refused anyway. The cache holds 4096 pairs and never forgets one early: if
someone floods it, new offers get "busy" (503) for a few minutes instead, so a
flood can delay transfers but cannot make a replay possible. Offers are also
rate-limited per IP address. Upload tokens are one-shot, expire after 10
minutes and are only accepted from the IP address that made the offer.
Replies are signed over a hash of the offer they answer, so an old reply
cannot be passed off as the answer to a new offer, and a captured body is
useless with any other transfer's key.

Keep the clocks of your computers roughly right (within 5 minutes); network
time does this for you on most systems.

### A file list that writes outside the download folder (path traversal, zip-slip)

The file list is checked before anything is asked or written: paths must be
relative and clean, with no `..`, no absolute, drive or UNC paths, no
backslashes, colons, NUL or control characters, no duplicates (case-blind) and
no file that is also a folder. The body carries only file bytes, in the
order and sizes of the signed list, so it cannot add entries, links or
devices. Each top-level item gets a fresh
name in the download folder, everything below it is joined and re-checked
under that folder, and Windows reserved names (CON, NUL, COM1, COM¹, ...),
trailing dots and spaces, control characters and invisible format characters
are made safe. The accept prompt shows the names as they will be written, so a
right-to-left override cannot make `invoice<RLO>fdp.exe` look like a PDF.
Files are written as `.rocket-part` and renamed when complete; a failed or
cancelled transfer removes what it wrote.

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

The drop page is **not encrypted**: browsers only allow the Web Crypto API
on secure (HTTPS) pages, and rocket serves plain HTTP. Files dropped from a
browser, and the cookie, cross the network in clear, so someone watching
could read or change them, or copy the cookie and drop files as that browser.
They still could not read anything from your computer. History marks drops
as not encrypted.

### An AI agent goes too far

The MCP server has exactly four tools: `list_peers`, `send_file`,
`get_status` and `list_history`. A test pins that set and checks no tool can
accept, trust, revoke, change settings or quit. `send_file` sends real files
from paths on this computer, so only connect agents you trust with your files.
Names of peers and files come from other computers: agents should treat them
as data, not instructions. See [mcp.md](mcp.md).

## Android

The Android app runs the same Go code as the desktop for everything on the
wire (`internal/receiver`, `internal/transfer`, `internal/protocol`, through
`mobile/rocketcore`): offer signing and checks, trust, the replay cache,
limits, manifest and path checks. The differences:

- **Listens only while you want it to.** The receiver, the beacon announce and
  the NSD advertisement run only while **Receive files** is on in the Receive
  tab, in a foreground service with an ongoing notification. Sending and
  discovery work with receiving off.
- **Only the LAN surface.** The phone serves `GET /api/hello`,
  `POST /api/offer`, `POST /api/recv` and `POST /api/recv/cancel`. There is
  no drop page, no control
  API and no UI over HTTP; the app itself is the UI. It listens on TCP 29005,
  or another free port when that is taken (announced in the beacon and NSD).
- **Offers.** Each offer raises a notification (Accept, Decline) and a sheet
  in the app (Accept, Always accept from this device, Decline). "Always
  accept" is only offered in the app, where the fingerprint is shown in full.
- **Files** go to `Download/rocket` through MediaStore (Android 10 and later)
  or the file system (Android 8 and 9, with `WRITE_EXTERNAL_STORAGE`). Paths
  are checked and made safe in Go before the app sees them; a cancelled or
  failed receive removes its pending entry.
- **Share sheet.** rocket sends only what another app shares as content
  (`content://` from that app's provider). `file://` links and rocket's own
  providers are refused, and so is a file that turns out to be in rocket's
  private storage, so no app can make rocket send its key or records.
- **Identity and records.** `node.key`, trusted senders and history live in
  the app's private storage. Backups and device-to-device transfers leave them
  out (`allowBackup="false"`, data extraction rules), so another device cannot
  inherit this phone's key; a reinstall or a new phone gets a new fingerprint.
- **Cleartext HTTP** is allowed by the app's network security config,
  because rocket's transport is plain HTTP on the LAN; the file bodies inside
  it are encrypted exactly as on the desktop.
- **Release APK.** `rocket-android.apk` is signed with the project's release
  key, which is kept offline and never in the repository or CI (CI builds an
  unsigned debug APK). Check the download against `SHA256SUMS`, and if you
  like, check the signer with
  `apksigner verify --print-certs rocket-android.apk`; the certificate SHA-256
  digest is
  `46:3A:4E:FF:1B:66:1D:BA:36:FB:68:8F:0E:A6:08:90:3F:17:7E:24:B8:D6:F5:59:78:26:0E:B7:89:F4:68:E4`
  (also in `android/release-cert.sha256`; the release scripts refuse an APK
  signed with any other certificate). Android refuses to install an update signed with any other key over the
  installed app.

## What the network can still see

rocket's transport is plain HTTP. Encryption covers the **contents of files**
sent between rocket apps. Anyone who can see the packets (shared Wi-Fi
without client isolation, a compromised router, a mirrored switch port) can
still:

- see file and folder **names, sizes and the folder layout** (the offer is
  signed, not encrypted),
- see **device names, fingerprints and versions** (discovery and hello are
  public), and which hosts send to which, when and how much,
- read and change files dropped from a **browser** (the drop page is not
  encrypted), and copy a drop cookie,
- block or cut off a transfer (the sender retries; the receiver keeps the
  authenticated part as a `.rocket-part` file for up to 24 hours so the
  transfer can resume, and never as a finished file).

They cannot read or undetectably change the contents of a transfer between
rocket apps, make your computer accept an offer from a trusted sender without
that sender's private key, or reach the local control API.

HTTPS with a local certificate authority was considered and left out: it
adds a certificate step on every host and phone. Signed offers and replies
with per-transfer keys give the same protection for file contents without
it. If names and sizes must stay private too, put the files in an encrypted
archive first.

## Other limits

- **Resume needs both ends.** Browser drops and uploads relayed from the
  browser UI do not resume, and the Android app does not keep a broken
  receive yet (the sender's retry starts that transfer over).
- **Unsigned binaries.** Release zips are not code-signed or notarized. Check
  them against `SHA256SUMS` from the release page. (The Android APK is signed;
  see [Android](#android).)
- **Firewall.** rocket needs TCP 29005 and UDP 29006 (and UDP 5353 for mDNS)
  open on private networks. Do not open them on public networks.
- **Your data folder** (`node.key`, `trusted.json`, `drop_tokens.json`,
  `settings.json`, `history.jsonl`, and the `resume` and `outgoing` records
  of interrupted transfers) is protected only by your user account's
  file permissions. Anyone who can read `node.key` can act as this host.

## Reporting a problem

Please open an issue at https://github.com/donaldsteele/rocket/issues. For
something sensitive, say so in the issue without the details and ask for a
private contact.
