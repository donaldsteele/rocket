package protocol

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/donaldsteele/rocket/internal/node"
)

// SigPrefix is the domain-separation prefix of the offer signing bytes.
const SigPrefix = "rocket-offer-v1\n"

// Signer is a node identity that can sign offers. *node.Identity implements it.
type Signer interface {
	Fingerprint() string
	PublicKeyB64() string
	Sign(msg []byte) []byte
}

// SigningBytes returns the exact bytes an offer signature covers:
//
//	"rocket-offer-v1\n" + receiver_fp + "\n" + sender_fp + "\n" + ts + "\n" +
//	nonce + "\n" + hex(sha256(canonical manifest JSON))
//
// ts is the decimal unix-seconds timestamp.
func SigningBytes(o *Offer) ([]byte, error) {
	mh, err := o.Manifest.Hash()
	if err != nil {
		return nil, err
	}
	s := SigPrefix + o.ReceiverFP + "\n" + o.SenderFP + "\n" + strconv.FormatInt(o.TS, 10) + "\n" + o.Nonce + "\n" + mh
	return []byte(s), nil
}

// NewNonce returns a fresh random nonce (32 hex chars).
func NewNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewOffer builds and signs an offer from id to the receiver with fingerprint
// receiverFP, timestamped now.
func NewOffer(id Signer, name, receiverFP string, m Manifest, now time.Time) (*Offer, error) {
	o := &Offer{
		Proto:      Version,
		SenderName: name,
		SenderFP:   id.Fingerprint(),
		SenderPub:  id.PublicKeyB64(),
		ReceiverFP: receiverFP,
		TS:         now.Unix(),
		Nonce:      NewNonce(),
		Manifest:   m,
	}
	if err := o.SignWith(id); err != nil {
		return nil, err
	}
	return o, nil
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

// CheckHello validates a hello reply: the protocol version, and that the
// fingerprint derives from the presented public key. If wantFP is not empty
// the hello must also carry exactly that fingerprint (the peer seen in
// discovery). It returns the receiver's public key.
func CheckHello(h Hello, wantFP string) (ed25519.PublicKey, error) {
	if h.Proto != Version {
		return nil, fmt.Errorf("%w: peer speaks %d, this host speaks %d", ErrProto, h.Proto, Version)
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

// Verify runs every v1 check on an offer: protocol version, the sender's
// public key and that its fingerprint derives from it, that the offer names
// this receiver, the timestamp window, nonce shape, manifest validity, the
// signature, and finally the replay cache (only offers with a good signature
// consume a nonce; ErrBusy when the cache is full of fresh nonces). It returns the sender's public key. Errors wrap one of the
// Err* values in this package.
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

// validNonce accepts 16 to 128 characters of [A-Za-z0-9_-], so a nonce can
// never smuggle a field separator into the signing bytes.
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
