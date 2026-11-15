package protocol

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/protocol/securestream"
)

// SigPrefix is the domain-separation prefix of the offer signing bytes.
const SigPrefix = "rocket-offer-v2\n"

// ReplySigPrefix is the domain-separation prefix of the reply signing bytes.
const ReplySigPrefix = "rocket-reply-v2\n"

// Signer is a node identity that can sign offers and replies.
// *node.Identity implements it.
type Signer interface {
	Fingerprint() string
	PublicKeyB64() string
	Sign(msg []byte) []byte
}

// SigningBytes returns the exact bytes an offer signature covers:
//
//	"rocket-offer-v2\n" + receiver_fp + "\n" + sender_fp + "\n" + ts + "\n" +
//	nonce + "\n" + eph + "\n" + hex(sha256(canonical manifest JSON))
//
// ts is the decimal unix-seconds timestamp, eph the base64 text as sent.
func SigningBytes(o *Offer) ([]byte, error) {
	mh, err := o.Manifest.Hash()
	if err != nil {
		return nil, err
	}
	s := SigPrefix + o.ReceiverFP + "\n" + o.SenderFP + "\n" + strconv.FormatInt(o.TS, 10) + "\n" + o.Nonce + "\n" + o.Eph + "\n" + mh
	return []byte(s), nil
}

// NewNonce returns a fresh random nonce (32 hex chars).
func NewNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewEphemeral returns a fresh X25519 key for one offer or one reply, and
// its public half as carried in "eph".
func NewEphemeral() (*ecdh.PrivateKey, string, error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	return k, base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

// DecodeEph parses an "eph" field: canonical standard base64 (with padding)
// of a 32-byte X25519 public key.
func DecodeEph(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 32 || base64.StdEncoding.EncodeToString(b) != s {
		return nil, ErrEph
	}
	if _, err := ecdh.X25519().NewPublicKey(b); err != nil {
		return nil, ErrEph
	}
	return b, nil
}

// NewOffer builds and signs an offer from id to the receiver with fingerprint
// receiverFP, timestamped now, with a fresh ephemeral key. It returns the
// private half of that key, which the sender needs for BodyKey.
func NewOffer(id Signer, name, receiverFP string, m Manifest, now time.Time) (*Offer, *ecdh.PrivateKey, error) {
	eph, ephB64, err := NewEphemeral()
	if err != nil {
		return nil, nil, err
	}
	o := &Offer{
		Proto:      Version,
		SenderName: name,
		SenderFP:   id.Fingerprint(),
		SenderPub:  id.PublicKeyB64(),
		ReceiverFP: receiverFP,
		TS:         now.Unix(),
		Nonce:      NewNonce(),
		Eph:        ephB64,
		Manifest:   m,
	}
	if err := o.SignWith(id); err != nil {
		return nil, nil, err
	}
	return o, eph, nil
}

// SignWith (re)computes o.Sig with id's key.
func (o *Offer) SignWith(id Signer) error {
	msg, err := SigningBytes(o)
	if err != nil {
		return err
	}
	o.Sig = base64.StdEncoding.EncodeToString(id.Sign(msg))
	return nil
}

// ReplySigningBytes returns the exact bytes a reply signature covers, for
// the offer whose signing bytes are offerSB:
//
//	"rocket-reply-v2\n" + hex(sha256(offerSB)) + "\n" + accepted ("1" or "0") +
//	"\n" + transfer_id + "\n" + token + "\n" + eph + "\n" + reason + "\n" +
//	resumeField
//
// resumeField is "entry:offset:sha256" (decimal numbers, lowercase hex) or
// "-" without a resume point. Empty fields are empty strings.
func ReplySigningBytes(offerSB []byte, r *OfferReply) []byte {
	sum := sha256.Sum256(offerSB)
	acc := "0"
	if r.Accepted {
		acc = "1"
	}
	s := ReplySigPrefix + hex.EncodeToString(sum[:]) + "\n" + acc + "\n" + r.TransferID + "\n" + r.Token + "\n" +
		r.Eph + "\n" + r.Reason + "\n" + resumeField(r.Resume)
	return []byte(s)
}

func resumeField(p *ResumePoint) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatUint(uint64(p.Entry), 10) + ":" + strconv.FormatUint(p.Offset, 10) + ":" + p.SHA256
}

// SignReply sets r.Sig: id's signature of ReplySigningBytes(offerSB, r).
func SignReply(id Signer, offerSB []byte, r *OfferReply) {
	r.Sig = base64.StdEncoding.EncodeToString(id.Sign(ReplySigningBytes(offerSB, r)))
}

// VerifyReply checks a reply to the offer whose signing bytes are offerSB:
// the shape of every signed field (so none can carry a field separator), and
// the signature by the receiver key pub (from its hello). An accepted reply
// must carry a token and a valid eph. Errors wrap ErrReply.
func VerifyReply(pub ed25519.PublicKey, offerSB []byte, r *OfferReply) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrReply, why) }
	sig, err := base64.StdEncoding.DecodeString(r.Sig)
	if r.Sig == "" || err != nil {
		return bad("no signature")
	}
	if !validNonce(r.TransferID) {
		return bad("bad transfer id")
	}
	if r.Token != "" && !validNonce(r.Token) {
		return bad("bad token")
	}
	if strings.ContainsAny(r.Reason, "\n\r") || len(r.Reason) > 256 {
		return bad("bad reason")
	}
	if r.Eph != "" {
		if _, err := DecodeEph(r.Eph); err != nil {
			return bad("bad ephemeral key")
		}
	}
	if r.Resume != nil && !validSHA256(r.Resume.SHA256) {
		return bad("bad resume point")
	}
	if !node.Verify(pub, ReplySigningBytes(offerSB, r), sig) {
		return bad("bad signature")
	}
	if r.Accepted && (r.Token == "" || r.Eph == "") {
		return bad("accepted without a token or key")
	}
	return nil
}

// validSHA256 accepts 64 lowercase hex characters.
func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// BodyKey derives the body key of transfer transferID offered by o (see
// securestream.DeriveKey): own is this side's ephemeral private key, peerEph
// the other side's "eph" (the reply's for the sender, the offer's for the
// receiver).
func BodyKey(own *ecdh.PrivateKey, peerEph string, o *Offer, transferID string) ([]byte, error) {
	peer, err := DecodeEph(peerEph)
	if err != nil {
		return nil, err
	}
	sb, err := SigningBytes(o)
	if err != nil {
		return nil, err
	}
	key, err := securestream.DeriveKey(own, peer, sb, o.SenderFP, o.ReceiverFP, transferID)
	if err != nil {
		// A small-order peer key (no shared secret) is a bad key.
		return nil, fmt.Errorf("%w: %v", ErrEph, err)
	}
	return key, nil
}

// OldPeerError is CheckHello's error for a peer that speaks another
// protocol version. It matches ErrProto.
type OldPeerError struct {
	Name  string // the peer's name, made safe to show
	Proto int    // the version it speaks
}

func (e *OldPeerError) Error() string {
	if e.Proto < Version {
		return fmt.Sprintf("%s runs an older rocket (protocol %d); update it to 1.1 or later", e.Name, e.Proto)
	}
	return fmt.Sprintf("%s runs a newer rocket (protocol %d); update this device", e.Name, e.Proto)
}

// Is makes errors.Is(err, ErrProto) true.
func (e *OldPeerError) Is(target error) bool { return target == ErrProto }

// CheckHello validates a hello reply: the protocol version (a peer of
// another version gets an *OldPeerError), and that the fingerprint derives
// from the presented public key. If wantFP is not empty the hello must also
// carry exactly that fingerprint (the peer seen in discovery). It returns
// the receiver's public key.
func CheckHello(h Hello, wantFP string) (ed25519.PublicKey, error) {
	if h.Proto != Version {
		proto := h.Proto
		if proto <= 0 {
			proto = 1 // a hello without a version is older still
		}
		return nil, &OldPeerError{Name: DisplayName(h.Name, "the other device"), Proto: proto}
	}
	pub, err := node.DecodePublicKey(h.Pub)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPublicKey, err)
	}
	if node.FingerprintOf(pub) != h.FP {
		return nil, ErrFingerprint
	}
	if wantFP != "" && h.FP != wantFP {
		return nil, fmt.Errorf("peer answered as %s, expected %s", h.FP, wantFP)
	}
	return pub, nil
}

// DisplayName makes an untrusted peer name safe to show: control and format
// characters dropped, whitespace collapsed, at most 64 runes; an empty
// result is fallback.
func DisplayName(s, fallback string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.Join(strings.Fields(s), " ") {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if n == 64 {
			break
		}
		b.WriteRune(r)
		n++
	}
	if b.Len() == 0 {
		return fallback
	}
	return b.String()
}

// Verifier checks incoming offers for one receiver. Safe for concurrent use.
type Verifier struct {
	selfFP string
	nonces *NonceCache
	// Now returns the current time; tests override it.
	Now func() time.Time
}

// NewVerifier returns a verifier for the receiver whose fingerprint is selfFP.
func NewVerifier(selfFP string) *Verifier {
	return &Verifier{selfFP: selfFP, nonces: NewNonceCache(NonceCacheSize), Now: time.Now}
}

// Verify runs every check on an offer, in this order: protocol version, the
// sender's public key and that its fingerprint derives from it, that the
// offer names this receiver, the timestamp window, nonce shape, the
// ephemeral key, manifest validity, the signature, and finally the replay
// cache (only offers with a good signature consume a nonce; ErrBusy when the
// cache is full of fresh nonces). It returns the sender's public key. Errors
// wrap one of the Err* values in this package.
func (v *Verifier) Verify(o *Offer) (ed25519.PublicKey, error) {
	if o.Proto != Version {
		return nil, fmt.Errorf("%w: got %d", ErrProto, o.Proto)
	}
	pub, err := node.DecodePublicKey(o.SenderPub)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPublicKey, err)
	}
	if node.FingerprintOf(pub) != o.SenderFP {
		return nil, ErrFingerprint
	}
	if o.ReceiverFP != v.selfFP {
		return nil, ErrWrongReceiver
	}
	now := v.Now()
	skew := now.Sub(time.Unix(o.TS, 0))
	if skew > MaxClockSkew || skew < -MaxClockSkew {
		return nil, fmt.Errorf("%w: off by %s", ErrStale, skew.Round(time.Second))
	}
	if !validNonce(o.Nonce) {
		return nil, ErrNonce
	}
	if _, err := DecodeEph(o.Eph); err != nil {
		return nil, err
	}
	if err := o.Manifest.Validate(); err != nil {
		return nil, err
	}
	sig, err := base64.StdEncoding.DecodeString(o.Sig)
	if err != nil {
		return nil, ErrSignature
	}
	msg, err := SigningBytes(o)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	if !node.Verify(pub, msg, sig) {
		return nil, ErrSignature
	}
	if err := v.nonces.Use(o.SenderFP+"/"+o.Nonce, now); err != nil {
		return nil, err
	}
	return pub, nil
}

// validNonce accepts 16 to 128 characters of [A-Za-z0-9_-], so a nonce (or a
// transfer ID or token in a reply) can never smuggle a field separator into
// the signing bytes.
func validNonce(n string) bool {
	if len(n) < 16 || len(n) > 128 {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}
