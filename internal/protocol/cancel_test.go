package protocol

import (
	"errors"
	"testing"
)

// vCancelSig is the sender's (seed 0x01) signature of the cancel signing
// bytes for vTransferID, computed with Node.js crypto.
const vCancelSig = "Ko3tmrJdiGRo7Q3Gx0gYxI28Z7j0T7W+YNSsXsuvg+jnlHHrtPsFl5HtvJ/vdMDtXfIHdp29s+vgu+nDF92dBQ=="

func TestCancelNoticeVector(t *testing.T) {
	snd := newSeedSigner(1)
	want := "rocket-cancel-v2\n" + vReceiverFP + "\n" + vSenderFP + "\n" + vTransferID
	if got := string(CancelSigningBytes(vReceiverFP, vSenderFP, vTransferID)); got != want {
		t.Fatalf("signing bytes %q", got)
	}
	c := SignCancel(snd, vReceiverFP, vTransferID)
	if c.Sig != vCancelSig || c.SenderFP != vSenderFP || c.TransferID != vTransferID {
		t.Fatalf("notice %+v", c)
	}
	if err := VerifyCancel(snd.pub(), vReceiverFP, &c); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(c *CancelNotice){
		"other transfer": func(c *CancelNotice) { c.TransferID = "00112233445566778899aabbccddeef0" },
		"bad id":         func(c *CancelNotice) { c.TransferID = "short" },
		"other sender":   func(c *CancelNotice) { c.SenderFP = vReceiverFP },
		"unsigned":       func(c *CancelNotice) { c.Sig = "" },
		"garbage sig":    func(c *CancelNotice) { c.Sig = "!!" },
	}
	for name, change := range bad {
		cc := c
		change(&cc)
		if err := VerifyCancel(snd.pub(), vReceiverFP, &cc); !errors.Is(err, ErrCancelNotice) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Addressed to another receiver, or checked against another key.
	if err := VerifyCancel(snd.pub(), vSenderFP, &c); !errors.Is(err, ErrCancelNotice) {
		t.Errorf("other receiver: %v", err)
	}
	if err := VerifyCancel(newSeedSigner(2).pub(), vReceiverFP, &c); !errors.Is(err, ErrCancelNotice) {
		t.Errorf("other key: %v", err)
	}
}
