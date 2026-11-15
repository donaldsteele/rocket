# rocket protocol v2

This is the wire protocol rocket hosts use to find each other and send files.
rocket 1.1 and later speak protocol 2; rocket 1.0 spoke protocol 1, and the
two do not talk to each other (see [Versions](#versions)).

The code that implements it is `internal/protocol` (types, signing bytes,
checks, manifest rules, the body key), `internal/protocol/securestream` (key
derivation and the encrypted stream), `internal/transfer` (sending and the
body format), `internal/receiver` (the peer-facing receive handlers, shared
by desktop and Android) and `internal/httpapi` (the desktop HTTP server).
Where this page and the code disagree, the code is right and this page has a
bug.

Everything runs on one TCP port (default **29005**) plus discovery on UDP
**29006** and mDNS (UDP 5353). The transport is plain HTTP, but every file
body between rocket apps is encrypted and authenticated end to end; offers,
replies and discovery are signed or public but not encrypted. See
[security.md](security.md) for what that means.

## Versions

| rocket | protocol |
|--------|----------|
| 1.0    | 1        |
| 1.1+   | 2        |

A protocol-2 host refuses protocol 1 in both directions, with a message
that says what to update:

- Sending to a host whose hello says `proto` 1 (or has no `proto`) fails
  before any offer is sent with
  `<name> runs an older rocket (protocol 1); update it to 1.1 or later`.
  A hello with a higher version fails with
  `<name> runs a newer rocket (protocol N); update this device`.
- An offer with `proto` 1 is answered **426 Upgrade Required** with the body
  `this device runs rocket 1.1 or later; update the sender`.

There is no plaintext fallback. Discovery tells hosts apart before anyone
tries (see `proto` below), so a peer list can show older peers as needing an
update.

## Identity

Each host has an ed25519 keypair, made on first run and kept in `node.key` in
the data folder (mode 0600).

- **pub**: the 32-byte public key, standard base64 (with padding).
- **fingerprint (fp)**: `hex(sha256(pub))[:16]`, 16 lowercase hex characters.
  It is the host's stable ID: trust survives an IP or name change.

The device name is a label chosen by the remote person. It is never trusted on
its own. The ed25519 key signs offers (sender) and replies (receiver); it
never encrypts anything. Encryption uses fresh X25519 keys made for each
offer and each reply and thrown away afterwards.

## Discovery

Two sources feed one registry keyed by fingerprint. A peer seen by both shows
`via: ["beacon","mdns"]`.

### UDP beacon (port 29006)

Every 2 seconds each host broadcasts one JSON datagram to
`255.255.255.255:29006`:

```json
{"name":"desk","fp":"0123456789abcdef","port":29005,"ver":"1.1.0","os":"windows","proto":2}
```

The receiver takes the peer's address from the datagram's source IP and the
`port` field. Packets with a port outside 1..65535, or with this host's own
fingerprint, are ignored. A beacon without `proto` (rocket 1.0) or with a
value below 1 counts as protocol 1. A beacon sighting expires after 7 seconds
of silence.

### mDNS (`_rocket._tcp.local`)

Each host advertises the service instance `<device name>._rocket._tcp.local`
on its HTTP port, with host name `rocket-<first 8 fp chars>.local` (so it never
clashes with the computer's own `.local` name) and TXT records:

| key     | value                       |
|---------|-----------------------------|
| `fp`    | fingerprint                 |
| `ver`   | rocket version              |
| `os`    | `windows`, `darwin`, `linux`, `android` |
| `proto` | `2`                         |

Hosts browse the same type and merge results by `fp`. A missing or
unreadable `proto` counts as 1; peers of every version are listed, each with
its `proto`, and only peers announcing 2 can be sent to. IPv4 addresses are
preferred and link-local (169.254.x) addresses are tried last. An mDNS
sighting expires after 75 seconds. `--no-mdns` turns mDNS off; the beacon
keeps running.

The peer lists (`GET /api/peers` on desktop, `PeersJSON` on Android) carry
`proto` for each peer. When both sources see a peer, the beacon's version
wins.

Discovery is only a hint. A peer is identified by the key it proves it holds
during a transfer, never by what discovery says.

## Sending: overview

```
sender                                   receiver
  GET  /api/hello  ───────────────────▶  {proto:2, name, fp, pub, version, os}
  POST /api/offer  (signed, eph_s) ───▶  verify, then prompt the person
                    ◀──────────────────  {accepted, transfer_id, token, eph_r, reason, sig}
       both derive key = HKDF(X25519(eph), offer, transfer_id)
  POST /api/recv?token=… (encrypted) ─▶  decrypt, check, write *.rocket-part, rename
                    ◀──────────────────  {"ok": true}
```

If the connection breaks after the offer was accepted, the receiver keeps
what arrived and the sender offers the same files again; the reply then
carries a resume point and only the missing bytes are sent (see
[Resume](#resume)). A sender that gives up a transfer tells the receiver
with a signed cancel notice (`POST /api/recv/cancel`), so nothing is kept.

A host can be addressed by its full fingerprint, by name or fingerprint prefix
(all from discovery), or directly as `host:port`. A full 16-hex fingerprint
matches only the peer with exactly that fingerprint, never a name (names are
not authenticated). A name or prefix must match exactly one peer; if several
match, the send fails rather than guess. The browser UI always names peers by
full fingerprint.

## GET /api/hello

```json
{"proto":2,"name":"desk","fp":"0123456789abcdef","pub":"<base64>","version":"1.1.0","os":"windows"}
```

The sender checks that `proto` is 2 (see [Versions](#versions)) and that
`fp` derives from `pub`. When it picked the peer from discovery it also
checks that `fp` is the fingerprint it expected, so a different host
answering on that address is refused. It keeps `pub`: the reply must be
signed with it.

## POST /api/offer

Body (at most 1 MiB):

```json
{
  "proto": 2,
  "sender_name": "laptop",
  "sender_fp": "fedcba9876543210",
  "sender_pub": "<base64 ed25519 public key>",
  "receiver_fp": "0123456789abcdef",
  "ts": 1794657600,
  "nonce": "3f7a9c2e4b1d8a6f0e5c7b9a2d4f6e8c",
  "eph": "<base64 X25519 public key, 32 bytes>",
  "manifest": {
    "kind": "multi",
    "entries": [
      {"path": "photos", "size": 0, "mode": 2147483648},
      {"path": "photos/beach.jpg", "size": 482113, "mode": 420}
    ],
    "total": 482113
  },
  "sig": "<base64 ed25519 signature>"
}
```

- `ts` is unix seconds.
- `nonce` is 16 to 128 characters of `[A-Za-z0-9_-]` (rocket sends 32 hex
  characters).
- `eph` is a fresh X25519 public key for this offer only, in canonical
  standard base64 with padding (44 characters). Every offer, including a
  retry of the same files, has a new one.
- `sig` is ed25519 by the sender's key over exactly these bytes (the **offer
  signing bytes**):

```
"rocket-offer-v2\n" + receiver_fp + "\n" + sender_fp + "\n" + ts + "\n" + nonce + "\n" + eph + "\n" + hex(sha256(canonical manifest JSON))
```

  `ts` is written in decimal and `eph` exactly as sent. The **canonical
  manifest JSON** is compact JSON with the fields in the order `kind`,
  `entries` (each `path`, `size`, `mode`), `total`; no HTML escaping, no
  trailing newline, and an empty entry list written as `[]`. In Go:
  `json.Encoder` with `SetEscapeHTML(false)` on the `protocol.Manifest`
  struct, trailing newline removed.

### Manifest

| field     | meaning |
|-----------|---------|
| `kind`    | `file`: one file with a bare name. `multi`: files and folders. Both use the same body. |
| `entries` | `path` (relative, `/`-separated), `size` in bytes, `mode` (Unix permission bits; bit 31 (`2147483648`) marks a folder). |
| `total`   | sum of all sizes. |

Rules (`Manifest.Validate`):

- `kind:file` has exactly one entry, not a folder, with a bare name (no `/`).
- `kind:multi` has 1 to 10,000 entries.
- `total` is 0..1 TiB and equals the sum of the sizes.
- Every path is non-empty, valid UTF-8, at most 4096 bytes (each segment at
  most 255), relative, already clean (no `.`, `..` or empty segments, no
  trailing `/`), and contains no NUL or control characters, no `\` and no `:`.
  This rules out absolute, volume (`C:\x`, `C:x`) and UNC (`//server/share`)
  paths and Windows alternate data streams.
- No duplicate paths, compared case-insensitively.
- No file is also the parent folder of another entry.
- Only permission bits and the folder bit may be set in `mode`; a folder has
  size 0.

A sender lists a folder's entries rooted at the folder's own name, each folder
before its contents. Symlinks and special files are skipped.

### Receiver checks

An offer whose `proto` is below 2 gets **426** (see [Versions](#versions)).
Otherwise the receiver answers **400** if any of these fail, in this order:

1. `proto` is 2.
2. `sender_pub` is a valid 32-byte key and `sender_fp == hex(sha256(pub))[:16]`.
3. `receiver_fp` is the receiver's own fingerprint.
4. `|now - ts| <= 5 minutes`.
5. The nonce has a valid shape.
6. `eph` is canonical base64 of 32 bytes.
7. The manifest is valid.
8. The signature verifies.
9. The pair `(sender_fp, nonce)` has not been used before (a cache of 4096
   recent nonces; a nonce is only spent once the signature is good).
10. The body key can be derived (an `eph` that gives an all-zero X25519
    result, a small-order point, is refused with "bad ephemeral key").

It answers **413** for a body over 1 MiB, and **429** when one IP sends offers
faster than a burst of 30 then one every 2 seconds, or when 16 offers are
already waiting for a decision, or 4 from the same IP (for a sender that would
be prompted; checked before the signature, so such an offer spends no nonce).
The body must arrive within 30 seconds. A nonce is remembered until it is
older than twice the clock-skew window and is never evicted early: when the
cache is full of fresh nonces the receiver answers **503** until room frees
up, so a flood of offers can delay offers but never make a replay possible.

### Decision

A sender is **trusted** when its fingerprint is in the trust store **and** its
public key equals the pinned key. If it is trusted and *auto-accept trusted
senders* is on (the default), the offer is accepted at once. An offer that
continues a kept transfer (same sender key, same manifest hash; see
[Resume](#resume)) is also accepted at once, whatever the setting: the
person already accepted it. Otherwise the person is asked (UI, tray badge,
phone notification) and the request long-polls for up to 2 minutes.

"Always accept from this host" pins `{fingerprint, name, pub, approved_at}`
in `trusted.json`.

## Reply

Every reply to a peer offer, accepted or not, is signed by the receiver's
ed25519 key:

```json
{"accepted":true,"transfer_id":"<id>","token":"<one-shot>","eph":"<base64 X25519>","sig":"<base64>"}
{"accepted":true,"transfer_id":"<id>","token":"<one-shot>","eph":"<base64 X25519>","resume":{"entry":3,"offset":1048576,"sha256":"<hex>"},"sig":"<base64>"}
{"accepted":false,"transfer_id":"<id>","reason":"declined","sig":"<base64>"}
{"accepted":false,"transfer_id":"<id>","reason":"timed out","sig":"<base64>"}
```

- `transfer_id` and `token` are 16 to 128 characters of `[A-Za-z0-9_-]`
  (rocket sends 32 hex characters). `token` is present only when accepted.
- `eph` is the receiver's fresh X25519 public key for this transfer, present
  only when accepted.
- `reason` explains a refusal; it never contains a line break.
- `resume` is present only when the receiver kept part of an earlier,
  interrupted transfer of the same files from the same sender: the body may
  then start at byte `offset` of entry `entry`, and `sha256` is the lowercase
  hex SHA-256 of bytes `[0, offset)` of that entry as the receiver holds
  them.

`sig` covers exactly these bytes (the **reply signing bytes**):

```
"rocket-reply-v2\n" + hex(sha256(offer signing bytes)) + "\n" + accepted ("1" or "0") + "\n" + transfer_id + "\n" + token + "\n" + eph + "\n" + reason + "\n" + resumeField
```

Absent fields are empty strings. `resumeField` is `entry:offset:sha256`
(decimal numbers) or `-` when there is no `resume`. Because the hash of the
offer signing bytes is inside, a reply belongs to exactly one offer (one
nonce, one sender `eph`).

The sender verifies the signature with the `pub` from hello (whose
fingerprint it already matched against the target) and the shape of every
field, and refuses an unsigned, badly signed or malformed reply, and an
accepted reply without `token` or `eph`, before it sends anything:
`peer <addr>: the receiver's reply is not validly signed: ...`.

## Body key

Both sides derive the same 32-byte key for one transfer:

```
shared = X25519(own eph private key, peer eph public key)        all-zero result refused
key    = HKDF-SHA256(ikm  = shared,
                     salt = sha256(offer signing bytes),
                     info = "rocket-v2 body\n" + sender_fp + "\n" + receiver_fp + "\n" + transfer_id)
```

The sender uses its offer key and the reply's `eph`, the receiver its reply
key and the offer's `eph`. Both ephemeral private keys exist only in memory
for one transfer. A retry is a new offer with new keys, so no key ever
protects two bodies.

## POST /api/recv?token=

- The token is one-shot, valid for 10 minutes, and only from the IP that made
  the offer. Otherwise: **403**.
- The body is the plaintext below, encrypted as a securestream under the
  body key. `Content-Length`, when sent, must be exactly the sealed size of
  the body for its start point: the whole transfer, or from the offered
  resume point (rocket always sends it); a chunked body is accepted too. The
  body may be at most the sealed size of the whole transfer.

### Plaintext

```
start header (16 bytes):  entry (uint32 BE) ‖ offset (uint64 BE) ‖ 4 zero bytes
then:                     the bytes of every file, in manifest order, from (entry, offset)
```

Folders contribute nothing; a file contributes exactly its manifest size.
There is no archive format: the signed manifest already fixes the names,
order and sizes. The start point must be `(0,0)` or exactly the `resume`
point the receiver offered in its reply; anything else (or non-zero padding)
is refused. `(0,0)` when a resume was offered means "start over": the
receiver drops what it kept and writes everything afresh.

### Securestream framing

The encrypted body is a sequence of frames:

```
frame  = uint32 BE ciphertext length ‖ ChaCha20-Poly1305 ciphertext (plaintext + 16-byte tag)
nonce  = uint64 BE frame counter ‖ 3 zero bytes ‖ last-flag byte
```

- A frame carries at most 64 KiB of plaintext, so the length is at most
  65552 (64 KiB + 16) and at least 16. rocket fills every frame but the
  last.
- The counter starts at 0 and goes up by one per frame. The last-flag byte is
  1 on the final frame and 0 on every other. The final frame may be empty; no
  other frame may be.
- There is no associated data.
- The reader treats as an error that aborts the transfer: a frame that fails
  authentication (wrong key, changed, reordered, replayed, dropped or moved
  frames all fail here), a length over 65552 or under 16, an empty frame that
  is not the final one, the end of the input before the final frame
  (truncation), any byte after the final frame, and counter exhaustion.
- Plaintext is released only from frames that authenticated, and the final
  frame's plaintext only once the input has ended cleanly behind it.

The sealed size of `n` plaintext bytes is `n + 20 × max(1, ceil(n / 65536))`.

### Storing and answers

The receiver writes each file to `<name>.rocket-part` and renames it when
complete. Each top-level name gets a fresh file or folder in the download
folder (`photos`, then `photos (1)` if taken), so nothing that was there before
is written into or over. Windows reserved names (including `COM¹`-style
superscript forms), trailing dots and spaces, control characters and invisible
format characters (bidi overrides, zero-width spaces) are sanitized, and a name
longer than 235 bytes is shortened (keeping its extension) so the part file
and a ` (n)` suffix still fit the 255-byte name limit. On a cancel (by
either side) and on any failure other than a network failure, including a
bad frame or a bad end after the last file was complete, everything the
transfer created is removed, including what an earlier attempt kept. After
a network failure the receiver keeps what arrived (see [Resume](#resume)).

Answers: **200** `{"ok":true}`; **400** when the body does not match the offer
(sizes, start point) or the stream is bad (authentication, truncation,
oversized frame, trailing data); **409** when the kept part a resume point
referred to is gone (discarded or being continued by another upload);
**410** when the receiving person cancelled, or the sender sent a cancel
notice; **500** on a disk error. The 410 may arrive while the sender is
still writing the body; it carries `Connection: close`, the receiver drops
the rest of the body for up to 2 s and then closes the connection, so the
sender should stop writing once it has the answer.

## Resume

A transfer that breaks for a **network reason** after the receiver accepted
it is continued instead of starting over.

### What counts as a network failure

The connection is refused, reset, closed or lost, a read or write times out,
or the body ends before its final frame (truncation). Both sides also treat
**60 seconds without a single byte moving** as a lost connection (the sender
watches what its upload writes, the receiver keeps a read deadline 60 s
ahead while data comes in), so a silent network drop is noticed too.

A cancel by either side, a decline, a frame that fails authentication, a
body that does not match the offer, a disk error and a failure reading the
sender's own files are **not** network failures: nothing is kept.

### Receiver: kept transfers

When a peer upload breaks for a network reason, the receiver keeps what it
stored, in place in the download folder: the folders made, the files
already complete (renamed to their final names) and the `.rocket-part` file
of the file in flight. Every byte kept was authenticated before it was
written. A record in `<data-dir>/resume/<id>.json` (mode 0600) holds the
sender's fingerprint and its public key at the time, the manifest and its
hash, the paths created, how many entries are complete, how many bytes of
the current one are stored, and the resume point with its hash. History
records the attempt as `"result": "interrupted"`.

A kept transfer lives **24 hours** from its last interruption. Expired ones
are removed with everything they kept, at start-up and every hour. It is
also removed when the transfer completes, when the person discards it, when
the sender sends a cancel notice, and when a later attempt fails for a
non-network reason or starts over.

The resume point the receiver offers is: the part file of the entry in
flight at its current length; or, when no file was in flight, the next
file entry at offset 0; or, when every file is complete and only the end of
the body is missing, the last file at its end. Its `sha256` is the SHA-256
of bytes `[0, offset)` of that entry as the receiver holds them.

### Receiver: answering an offer

An offer whose sender key (`sender_pub`, exactly) and manifest hash match a
live kept transfer, after its signature checks out, is accepted at once
with `resume` in the signed reply. Any other sender, or other files, gets
the normal decision; a decline leaves the kept transfer alone.

If an upload from the same sender key of the same manifest is still being
received when such an offer arrives (this side has not seen the old
connection break), the receiver waits up to 3 s for it to end. If it moved
no byte in that time it is stopped as a lost connection and kept; then the
offer is answered with its resume point. An upload that is still moving is
left alone and the offer is handled normally.

### Body start

The body of an accepted resume offer starts with either the offered resume
point or `(0,0)`:

- At the resume point, the receiver continues: complete entries are taken
  as they are, the part file is cut to `offset` and the rest appended.
- At `(0,0)`, the receiver discards what it kept and writes everything
  afresh (the sender's file changed).
- Anything else is refused (400) and the kept transfer is dropped.

### Sender

- **Resume point check.** The sender hashes bytes `[0, offset)` of its own
  file for the offered entry. When the hash matches, the body starts at the
  resume point; otherwise at `(0,0)`. Files before the point are trusted as
  they are: their sizes match the manifest and the receiver authenticated
  their content when it got them.
- **Retries.** On a network failure after an accepted offer, the sender
  offers the same manifest again (new hello, new offer with a new nonce and
  `eph`, so new keys) after **2, 5, 10, 20 and 30 seconds**. Meanwhile the
  transfer shows as `reconnecting` with `attempt`/`attempts` ("Reconnecting
  2/5"), and a `retrying` event goes out. A retry that is declined, or fails
  for any other reason, ends the send.
- **Interrupted.** When the fifth retry fails too, the send ends with
  `"result": "interrupted"` and is kept for 24 hours in
  `<data-dir>/outgoing/<id>.json` with what is needed to send it again: the
  peer, the manifest hash, and on the desktop the local paths. The person
  can then **Resume** (the files are listed again; unchanged files continue
  where they stopped) or **Discard** it.
- Sends whose content cannot be read twice (a browser upload relayed by the
  desktop UI) make one attempt and ignore resume points; when they fail
  they send a cancel notice so the receiver keeps nothing.

The Android app sends the same way, keeping the content URIs for Resume
(with persistable read access where the provider allows it, otherwise
Resume asks for the files again). It keeps a broken receive the same way
too: the finished files and the incomplete one stay in `Download/rocket`
(the incomplete one as a pending MediaStore item, invisible to other apps),
and the sender's next offer continues it.

## POST /api/recv/cancel

The sender tells the receiver that it gave up a transfer it had an
accepted offer for, so that the receiver removes what it stored or kept:

```json
{"transfer_id":"<the receiver's transfer_id>","sender_fp":"fedcba9876543210","sig":"<base64>"}
```

`sig` is ed25519 by the sender's key over exactly these bytes (the **cancel
signing bytes**):

```
"rocket-cancel-v2\n" + receiver_fp + "\n" + sender_fp + "\n" + transfer_id
```

The receiver checks it against the key of the offer that transfer belongs
to. Then:

- an upload still being received stops, removes everything it stored (an
  earlier attempt's included) and is recorded as cancelled by the sender;
- an accepted offer whose upload has not started loses its token;
- a kept transfer that this transfer created or was offered to continue is
  discarded with what it kept.

Answers: **200** `{"ok":true}`; **400** for a malformed notice (at most
4 KiB); **403** for a bad signature; **404** when nothing of that transfer is
left; **429** when rate-limited like offers.

rocket sends a cancel notice when the person cancels a send (during the
body: before the body is cut off, so the receiver can tell a cancel from a
broken connection; while reconnecting: for the last accepted attempt), when
reading its own files fails, when a send that cannot retry hits a network
failure, and when the person discards an interrupted send. It is best
effort, at most 3 seconds: a receiver that cannot be reached removes its
kept transfer after 24 hours.

## Resume on the control surface

The desktop's loopback-only control API (same rules as every local route:
loopback, Host allowlist, `X-Rocket-CSRF: 1` on POST; see
[security.md](security.md#two-surfaces-on-one-port)) has:

| route | does |
|-------|------|
| `GET /api/resumable` | `{"outgoing": [interrupted sends], "incoming": [transfers kept here]}` |
| `POST /api/resume {transfer_id}` | sends an interrupted send again; answers `{ok, transfer_id}` with the new transfer's ID |
| `POST /api/discard {transfer_id}` | forgets an interrupted send (and sends its receiver a cancel notice) or removes a kept transfer with its files |

An outgoing item is `{transfer_id, name, peer_name, peer_fp, addr, files,
bytes, manifest_hash, paths, receiver_fp, remote_id, error, interrupted,
expires}`; an incoming one `{id, name, sender_name, sender_fp, files,
total, done, location, kept_at, expires}`. Events: `retrying`, `error` with
`result: "interrupted"`, `kept` (an incoming item) and `kept_gone` `{id,
reason}`. The Android core has the same as `ResumableJSON`, `Resume` and
`Discard` (`mobile/rocketcore`).

## Test vectors

Known-answer tests pin every byte of the format; the values were computed
independently of rocket (Node.js crypto). With the sender's ed25519 seed 32
bytes of `0x01`, the receiver's 32 bytes of `0x02`, and the X25519 keys of
RFC 7748 section 6.1 (Alice for the offer, Bob for the reply):

```
sender fp          34750f98bd59fcfc
receiver fp        6a3803d5f059902a
offer eph          hSDwCYkwp1R0i33ctD73Wg2/Og0mOBr066SpjqqbTmo=
reply eph          3p7bfXt9wbTTW2HC7OQ1Nz+DQ8hbeGdNrfx+FG+IK08=
manifest           {"kind":"multi","entries":[{"path":"photos","size":0,"mode":2147483648},{"path":"photos/beach.jpg","size":5,"mode":420}],"total":5}
ts, nonce          1794657600, 3f7a9c2e4b1d8a6f0e5c7b9a2d4f6e8c
offer sig          PjyhEPF5POTHYN651NLgUJFs9hMN9Jrr57AitWlYtRDZm9sZ+fvHim8n3rIb+Vn/XKrabhRh0dWNzJjRZ+6gCQ==
transfer_id, token 00112233445566778899aabbccddeeff, ffeeddccbbaa99887766554433221100
reply sig          jq6WeOpRgkiOfWxYwM9d9n/DPc/xuHlS+X6IovtraMzwo4FN3SExxJSlors6eesqXNJrF2zWGnQm5nbxGzcrCg==
X25519 shared      4a5d9d5ba4ce2de1728e3bf480350f25e07e21c947d19e3376f09b3c1e161742
HKDF salt          561837af2b824379813ec41ef8e019d57f4a5a1be5165271aef733f3ea4b0e16
body key           d95038866247104d7dbb9ce4dd110cc213d8e0d1d5aa91631161f43ad54f89ed
stream of header (0,0) + "hello"
                   000000251f9ce57c0ef08e5fd2dbb96f24c7cd1f480b167c6e5789c19c370c1dbcbf997ece2bab6d77
```

The full set (declined and resume replies, empty and multi-frame streams)
is in `internal/protocol/v2_test.go` and
`internal/protocol/securestream/securestream_test.go`. The cancel notice for
the same transfer ID, signed by the sender, is in
`internal/protocol/cancel_test.go`:

```
cancel sig         Ko3tmrJdiGRo7Q3Gx0gYxI28Z7j0T7W+YNSsXsuvg+jnlHHrtPsFl5HtvJ/vdMDtXfIHdp29s+vgu+nDF92dBQ==
```

## Browser drop (LAN guests)

The drop page keeps the 1.0 flow: plain HTTP, no encryption (browsers allow
WebCrypto only on secure origins, and the page is served over plain HTTP),
no signatures. A browser on another computer that opens
`http://<host>:29005/` gets the drop page (`drop.html`), not the full UI.

1. `POST /drop/offer` with `{"files":[{"name":"a.jpg","size":1234}, …]}`. Names
   are reduced to safe base names (repeats are numbered `a (1).jpg`). The
   request long-polls for the person's decision, like a peer offer, and
   answers `{accepted, token, transfer_id, reason}` (no `eph`, `resume` or
   `sig`). On the first accept it
   sets the cookie `rocket_drop` (HttpOnly, SameSite=Strict, 30 days). A
   browser that sends a valid `rocket_drop` cookie skips the prompt while
   auto-accept is on. The server stores only the SHA-256 of each cookie token
   (`drop_tokens.json`).
2. `POST /drop/upload?token=` with a `multipart/form-data` body: one part per
   offered file, in offer order, each with the offered size. The server streams
   each part to disk; it never buffers the whole form.

History entries of drops have `"encrypted": false`; every peer transfer's
entry has `"encrypted": true`.

## Errors and limits at a glance

| limit                         | value      |
|-------------------------------|------------|
| offer body                    | 1 MiB      |
| entries per offer             | 10,000     |
| bytes per offer               | 1 TiB      |
| clock skew                    | 5 minutes  |
| nonce cache                   | 4096       |
| decision wait                 | 2 minutes  |
| upload token lifetime         | 10 minutes |
| pending offers (total / per IP) | 16 / 4   |
| offers per IP                 | burst 30, then 1 per 2 s |
| offer body arrival            | 30 seconds |
| connections per LAN IP        | 32         |
| drop cookie lifetime          | 30 days    |
| plaintext per frame           | 64 KiB     |
| ciphertext length per frame   | 16 .. 65552 bytes |
| sender retries                | 5, after 2, 5, 10, 20, 30 s |
| no byte moving (lost connection) | 60 seconds |
| kept transfer / interrupted send | 24 hours, swept hourly |
| cancel notice body            | 4 KiB      |
