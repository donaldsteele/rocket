# rocket protocol v1

This is the wire protocol rocket hosts use to find each other and send files.
The code that implements it is `internal/protocol` (types, signing bytes,
checks, manifest rules), `internal/transfer` (sending and receiving) and
`internal/httpapi` (the HTTP handlers). Where this page and the code disagree,
the code is right and this page has a bug.

Everything runs on one TCP port (default **29005**) plus discovery on UDP
**29006** and mDNS (UDP 5353). Traffic is plain HTTP: see
[security.md](security.md) for what that means.

## Identity

Each host has an ed25519 keypair, made on first run and kept in `node.key` in
the data folder (mode 0600).

- **pub**: the 32-byte public key, standard base64 (with padding).
- **fingerprint (fp)**: `hex(sha256(pub))[:16]`, 16 lowercase hex characters.
  It is the host's stable ID: trust survives an IP or name change.

The device name is a label chosen by the remote person. It is never trusted on
its own.

## Discovery

Two sources feed one registry keyed by fingerprint. A peer seen by both shows
`via: ["beacon","mdns"]`.

### UDP beacon (port 29006)

Every 2 seconds each host broadcasts one JSON datagram to
`255.255.255.255:29006`:

```json
{"name":"desk","fp":"0123456789abcdef","port":29005,"ver":"1.0.0","os":"windows"}
```

The receiver takes the peer's address from the datagram's source IP and the
`port` field. Packets with a port outside 1..65535, or with this host's own
fingerprint, are ignored. A beacon sighting expires after 7 seconds of silence.

### mDNS (`_rocket._tcp.local`)

Each host advertises the service instance `<device name>._rocket._tcp.local`
on its HTTP port, with host name `rocket-<first 8 fp chars>.local` (so it never
clashes with the computer's own `.local` name) and TXT records:

| key     | value                       |
|---------|-----------------------------|
| `fp`    | fingerprint                 |
| `ver`   | rocket version              |
| `os`    | `windows`, `darwin`, `linux` |
| `proto` | `1`                         |

Hosts browse the same type and merge results by `fp`. IPv4 addresses are
preferred and link-local (169.254.x) addresses are tried last. An mDNS
sighting expires after 75 seconds. `--no-mdns` turns mDNS off; the beacon
keeps running.

Discovery is only a hint. A peer is identified by the key it proves it holds
during a transfer, never by what discovery says.

## Sending: overview

```
sender                                   receiver
  GET  /api/hello  ───────────────────▶  {proto, name, fp, pub, version, os}
  POST /api/offer  (signed) ──────────▶  verify, then prompt the person
                    ◀──────────────────  {accepted, token, transfer_id, reason}
  POST /api/recv?token=… (body) ──────▶  stream to *.rocket-part, check, rename
                    ◀──────────────────  {"ok": true}
```

A host can be addressed by name, by fingerprint prefix (both from discovery),
or directly as `host:port`.

## GET /api/hello

```json
{"proto":1,"name":"desk","fp":"0123456789abcdef","pub":"<base64>","version":"1.0.0","os":"windows"}
```

The sender checks that `proto` is 1 and that `fp` derives from `pub`. When it
picked the peer from discovery it also checks that `fp` is the fingerprint it
expected, so a different host answering on that address is refused.

## POST /api/offer

Body (at most 1 MiB):

```json
{
  "proto": 1,
  "sender_name": "laptop",
  "sender_fp": "fedcba9876543210",
  "sender_pub": "<base64 ed25519 public key>",
  "receiver_fp": "0123456789abcdef",
  "ts": 1794657600,
  "nonce": "3f7a9c2e4b1d8a6f0e5c7b9a2d4f6e8c",
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
- `sig` is ed25519 by the sender's key over exactly these bytes:

```
"rocket-offer-v1\n" + receiver_fp + "\n" + sender_fp + "\n" + ts + "\n" + nonce + "\n" + hex(sha256(canonical manifest JSON))
```

  `ts` is written in decimal. The **canonical manifest JSON** is compact JSON
  with the fields in the order `kind`, `entries` (each `path`, `size`, `mode`),
  `total`; no HTML escaping, no trailing newline, and an empty entry list
  written as `[]`. In Go: `json.Encoder` with `SetEscapeHTML(false)` on the
  `protocol.Manifest` struct, trailing newline removed.

### Manifest

| field     | meaning |
|-----------|---------|
| `kind`    | `file`: one file, raw body. `multi`: files and folders, tar body. |
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

The receiver answers **400** if any of these fail, in this order:

1. `proto` is 1.
2. `sender_pub` is a valid 32-byte key and `sender_fp == hex(sha256(pub))[:16]`.
3. `receiver_fp` is the receiver's own fingerprint.
4. `|now - ts| <= 5 minutes`.
5. The nonce has a valid shape.
6. The manifest is valid.
7. The signature verifies.
8. The pair `(sender_fp, nonce)` has not been used before (an LRU of 4096
   recent nonces; a nonce is only spent once the signature is good).

It answers **413** for a body over 1 MiB and **429** when 16 offers are
already waiting for a decision, or 4 from the same IP.

### Decision

A sender is **trusted** when its fingerprint is in the trust store **and** its
public key equals the pinned key. If it is trusted and *auto-accept trusted
senders* is on (the default), the offer is accepted at once. Otherwise the
person is asked (UI, tray badge) and the request long-polls for up to 2
minutes.

Reply:

```json
{"accepted": true,  "token": "<one-shot>", "transfer_id": "<id>"}
{"accepted": false, "transfer_id": "<id>", "reason": "declined"}
{"accepted": false, "transfer_id": "<id>", "reason": "timed out"}
```

"Always accept from this host" pins `{fingerprint, name, pub, approved_at}`
in `trusted.json`.

## POST /api/recv?token=

- The token is one-shot, valid for 10 minutes, and only from the IP that made
  the offer. Otherwise: **403**.
- `kind:file`: the body is the raw file. `Content-Length` must equal the
  manifest total.
- `kind:multi`: the body is a tar stream (`archive/tar`, USTAR or PAX headers)
  whose entries match the manifest exactly, in order: a folder is a
  `TypeDir` entry named `<path>/`, a file a `TypeReg` entry named `<path>` with
  the manifest size. Any other entry type (symlink, hard link, device), a
  wrong name, size, order or count aborts the transfer. The body may be at most
  the total plus room for tar framing.

The receiver writes each file to `<name>.rocket-part` and renames it when
complete. Each top-level name gets a fresh file or folder in the download
folder (`photos`, then `photos (1)` if taken), so nothing that was there before
is written into or over. Windows reserved names and trailing dots and spaces
are sanitized. On any failure, or on cancel, everything the transfer created is
removed.

Answers: **200** `{"ok":true}`; **400** on a size or archive mismatch; **410**
when the receiving person cancelled; **500** on a disk error.

## Browser drop (LAN guests)

A browser on another computer that opens `http://<host>:29005/` gets the drop
page (`drop.html`), not the full UI.

1. `POST /drop/offer` with `{"files":[{"name":"a.jpg","size":1234}, …]}`. Names
   are reduced to safe base names (repeats are numbered `a (1).jpg`). The
   request long-polls for the person's decision, like a peer offer, and
   answers `{accepted, token, transfer_id, reason}`. On the first accept it
   sets the cookie `rocket_drop` (HttpOnly, SameSite=Strict, 30 days). A
   browser that sends a valid `rocket_drop` cookie skips the prompt while
   auto-accept is on. The server stores only the SHA-256 of each cookie token
   (`drop_tokens.json`).
2. `POST /drop/upload?token=` with a `multipart/form-data` body: one part per
   offered file, in offer order, each with the offered size. The server streams
   each part to disk; it never buffers the whole form.

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
| drop cookie lifetime          | 30 days    |
