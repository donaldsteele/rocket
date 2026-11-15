// Package protocol is the single source of truth for rocket's peer wire
// protocol, version 2: the hello, offer and reply messages, the canonical
// bytes the offer and reply signatures cover, signature and freshness
// checks, the replay (nonce) cache, manifest validation, and the body key
// (BodyKey; the stream itself is in package securestream).
//
// A transfer is a two-phase push. The sender fetches GET /api/hello to learn
// the receiver's fingerprint and public key, then POSTs a signed Offer with
// a fresh ephemeral X25519 key to /api/offer. The receiver verifies the offer
// (Verifier.Verify), prompts its person unless the sender is trusted, and
// replies with an OfferReply signed by its own key (SignReply), holding its
// own ephemeral key and a one-shot upload token for POST /api/recv. Both
// sides derive the body key from the two ephemeral keys (BodyKey), and the
// body travels as a securestream.
package protocol

import (
	"errors"
	"time"
)

// Version is the protocol version carried in every hello and offer.
const Version = 2

// Proto is another name for Version.
const Proto = Version

// Limits and windows of the protocol.
const (
	MaxOfferBytes  = 1 << 20         // offer request body cap
	MaxEntries     = 10000           // manifest entries
	MaxTotal       = int64(1) << 40  // 1 TiB per offer
	MaxClockSkew   = 5 * time.Minute // |now - ts| allowed
	NonceCacheSize = 4096            // replay LRU capacity
	MaxPathBytes   = 4096            // one manifest path
	MaxSegment     = 255             // one path segment, in bytes
	OfferWait      = 2 * time.Minute // receiver long-poll for a decision
	TokenTTL       = 10 * time.Minute
	MaxPending     = 16 // pending offers in total
	MaxPendingIP   = 4  // pending offers per remote IP
)

// Manifest kinds.
const (
	KindFile  = "file"  // one file
	KindMulti = "multi" // several files and folders
)

// ModeDir marks a directory entry in Entry.Mode (same bit as io/fs.ModeDir).
// A directory entry has size 0.
const ModeDir uint32 = 1 << 31

// Hello is the reply to GET /api/hello.
type Hello struct {
	Proto   int    `json:"proto"`
	Name    string `json:"name"`
	FP      string `json:"fp"`
	Pub     string `json:"pub"` // base64 ed25519 public key
	Version string `json:"version"`
	OS      string `json:"os"`
}

// Entry is one file or directory in a manifest. Path is relative, uses "/"
// separators and is clean (see ValidatePath). Mode holds Unix permission
// bits, plus ModeDir for a directory.
type Entry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Mode uint32 `json:"mode"`
}

// Manifest describes everything an offer will send.
type Manifest struct {
	Kind    string  `json:"kind"`
	Entries []Entry `json:"entries"`
	Total   int64   `json:"total"`
}

// Offer is the body of POST /api/offer.
type Offer struct {
	Proto      int      `json:"proto"`
	SenderName string   `json:"sender_name"`
	SenderFP   string   `json:"sender_fp"`
	SenderPub  string   `json:"sender_pub"` // base64 ed25519 public key
	ReceiverFP string   `json:"receiver_fp"`
	TS         int64    `json:"ts"` // unix seconds
	Nonce      string   `json:"nonce"`
	Eph        string   `json:"eph"` // base64 ephemeral X25519 public key, fresh per offer
	Manifest   Manifest `json:"manifest"`
	Sig        string   `json:"sig"` // base64 ed25519 signature of SigningBytes
}

// OfferReply is the receiver's answer to an offer. A peer reply (POST
// /api/offer) is always signed by the receiver (SignReply); Eph is set when
// it is accepted, Resume only when the receiver kept a partial transfer that
// this offer may continue. The browser drop flow (POST /drop/offer) uses the
// same shape without Eph, Resume and Sig.
type OfferReply struct {
	Accepted   bool         `json:"accepted"`
	TransferID string       `json:"transfer_id,omitempty"`
	Token      string       `json:"token,omitempty"`  // one-shot token for /api/recv
	Eph        string       `json:"eph,omitempty"`    // base64 ephemeral X25519 public key of the receiver
	Reason     string       `json:"reason,omitempty"` // ReasonDeclined, ReasonTimedOut, ...
	Resume     *ResumePoint `json:"resume,omitempty"`
	Sig        string       `json:"sig,omitempty"` // base64 ed25519 signature of ReplySigningBytes
}

// ResumePoint is where a body may start instead of the beginning: byte
// Offset of manifest entry Entry. SHA256 is the lowercase hex SHA-256 of
// bytes [0, Offset) of that entry as the receiver holds them.
type ResumePoint struct {
	Entry  uint32 `json:"entry"`
	Offset uint64 `json:"offset"`
	SHA256 string `json:"sha256"`
}

// OfferReply reasons for a refusal.
const (
	ReasonDeclined = "declined"
	ReasonTimedOut = "timed out"
)

// Verification errors. Verify wraps one of these, so callers can use
// errors.Is; every one of them maps to HTTP 400.
var (
	ErrProto         = errors.New("unsupported protocol version")
	ErrPublicKey     = errors.New("bad sender public key")
	ErrFingerprint   = errors.New("sender fingerprint does not match its public key")
	ErrWrongReceiver = errors.New("offer is addressed to a different receiver")
	ErrStale         = errors.New("offer timestamp outside the allowed window")
	ErrNonce         = errors.New("bad nonce")
	ErrReplay        = errors.New("offer nonce already used")
	ErrBusy          = errors.New("too many recent offers; try again in a few minutes")
	ErrSignature     = errors.New("bad offer signature")
	ErrManifest      = errors.New("invalid manifest")
	ErrEph           = errors.New("bad ephemeral key")
	ErrReply         = errors.New("the receiver's reply is not validly signed")
)

// OldSenderMsg is the body of the 426 answer to an offer of an older
// protocol.
const OldSenderMsg = "this device runs rocket 1.1 or later; update the sender"
