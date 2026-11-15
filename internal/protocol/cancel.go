package protocol

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/donaldsteele/rocket/internal/node"
)

// CancelSigPrefix is the domain-separation prefix of the cancel signing
// bytes.
const CancelSigPrefix = "rocket-cancel-v2\n"

// ErrCancelNotice is wrapped by VerifyCancel.
var ErrCancelNotice = errors.New("bad cancel notice")

// CancelNotice is the body of POST /api/recv/cancel: the sender tells the
// receiver that it gave up transfer TransferID (the receiver's transfer ID
// from an accepted reply), so the receiver keeps nothing of it for a resume.
// Sig is the sender's ed25519 signature of CancelSigningBytes.
type CancelNotice struct {
	TransferID string `json:"transfer_id"`
	SenderFP   string `json:"sender_fp"`
	Sig        string `json:"sig"`
}

// CancelSigningBytes returns the exact bytes a cancel notice signature
// covers:
//
//	"rocket-cancel-v2\n" + receiver_fp + "\n" + sender_fp + "\n" + transfer_id
func CancelSigningBytes(receiverFP, senderFP, transferID string) []byte {
	return []byte(CancelSigPrefix + receiverFP + "\n" + senderFP + "\n" + transferID)
}

// SignCancel builds the cancel notice for transfer transferID of the
// receiver with fingerprint receiverFP, signed by id.
func SignCancel(id Signer, receiverFP, transferID string) CancelNotice {
	c := CancelNotice{TransferID: transferID, SenderFP: id.Fingerprint()}
	c.Sig = base64.StdEncoding.EncodeToString(id.Sign(CancelSigningBytes(receiverFP, c.SenderFP, transferID)))
	return c
}

// VerifyCancel checks a cancel notice addressed to the receiver receiverFP
// against the public key pub of the sender of that transfer (pinned when
// the receiver accepted its offer). Errors wrap ErrCancelNotice.
func VerifyCancel(pub ed25519.PublicKey, receiverFP string, c *CancelNotice) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrCancelNotice, why) }
	if !validNonce(c.TransferID) {
		return bad("bad transfer id")
	}
	if c.SenderFP != node.FingerprintOf(pub) {
		return bad("not the sender of this transfer")
	}
	sig, err := base64.StdEncoding.DecodeString(c.Sig)
	if c.Sig == "" || err != nil {
		return bad("no signature")
	}
	if !node.Verify(pub, CancelSigningBytes(receiverFP, c.SenderFP, c.TransferID), sig) {
		return bad("bad signature")
	}
	return nil
}

// ValidID reports whether s has the shape of a transfer ID, token or nonce:
// 16 to 128 characters of [A-Za-z0-9_-].
func ValidID(s string) bool { return validNonce(s) }
