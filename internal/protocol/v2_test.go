package protocol

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/node"
)

// Protocol v2 vectors, computed independently with Node.js crypto. The
// sender's ed25519 seed is 32 bytes of 0x01, the receiver's 32 bytes of 0x02;
// the ephemeral X25519 keys are Alice's (offer) and Bob's (reply) from
// RFC 7748, section 6.1.
const (
	vSenderPub   = "iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w="
	vSenderFP    = "34750f98bd59fcfc"
	vReceiverPub = "gTl3Dqh9F19Wo1Rmw0x+zMuNipG07jeiXfYPW4/Js5Q="
	vReceiverFP  = "6a3803d5f059902a"
	vEphA        = "hSDwCYkwp1R0i33ctD73Wg2/Og0mOBr066SpjqqbTmo="
	vEphB        = "3p7bfXt9wbTTW2HC7OQ1Nz+DQ8hbeGdNrfx+FG+IK08="
	vEphAPriv    = "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a"
	vEphBPriv    = "5dab087e624a8a4b79e17f8b83800ee66f3bb1292618b6fd1c2f8b27ff88e0eb"
	vManifest    = `{"kind":"multi","entries":[{"path":"photos","size":0,"mode":2147483648},{"path":"photos/beach.jpg","size":5,"mode":420}],"total":5}`
	vManifestH   = "e134e1fa49cbdca1e8f8a167f8374acc503e894760b40ab8b6e3b6b8ed81477e"
	vTS          = 1794657600
	vNonce       = "3f7a9c2e4b1d8a6f0e5c7b9a2d4f6e8c"
	vOfferSB     = "rocket-offer-v2\n6a3803d5f059902a\n34750f98bd59fcfc\n1794657600\n3f7a9c2e4b1d8a6f0e5c7b9a2d4f6e8c\nhSDwCYkwp1R0i33ctD73Wg2/Og0mOBr066SpjqqbTmo=\ne134e1fa49cbdca1e8f8a167f8374acc503e894760b40ab8b6e3b6b8ed81477e"
	vOfferSig    = "PjyhEPF5POTHYN651NLgUJFs9hMN9Jrr57AitWlYtRDZm9sZ+fvHim8n3rIb+Vn/XKrabhRh0dWNzJjRZ+6gCQ=="
	vTransferID  = "00112233445566778899aabbccddeeff"
	vToken       = "ffeeddccbbaa99887766554433221100"
	vReplySB     = "rocket-reply-v2\n561837af2b824379813ec41ef8e019d57f4a5a1be5165271aef733f3ea4b0e16\n1\n00112233445566778899aabbccddeeff\nffeeddccbbaa99887766554433221100\n3p7bfXt9wbTTW2HC7OQ1Nz+DQ8hbeGdNrfx+FG+IK08=\n\n-"
	vReplySig    = "jq6WeOpRgkiOfWxYwM9d9n/DPc/xuHlS+X6IovtraMzwo4FN3SExxJSlors6eesqXNJrF2zWGnQm5nbxGzcrCg=="
	vResumeSB    = "rocket-reply-v2\n561837af2b824379813ec41ef8e019d57f4a5a1be5165271aef733f3ea4b0e16\n1\n00112233445566778899aabbccddeeff\nffeeddccbbaa99887766554433221100\n3p7bfXt9wbTTW2HC7OQ1Nz+DQ8hbeGdNrfx+FG+IK08=\n\n1:1048576:abababababababababababababababababababababababababababababababab"
	vResumeSig   = "MMveqsA4iKNh2+WDXheK2/q7zmzYtlUAvCzbA57Lj2liN4vVXaR/e8+HLgjQaqRIJTdIa9kqHuYNP3IX6kj8AA=="
	vDeclineSB   = "rocket-reply-v2\n561837af2b824379813ec41ef8e019d57f4a5a1be5165271aef733f3ea4b0e16\n0\n00112233445566778899aabbccddeeff\n\n\ndeclined\n-"
	vDeclineSig  = "bkcToBh4FZCBv+Hmpa51fGC5VZd1zMkZQaG7wCr5fNLDq42DLziTK2u1waIjkgkaaN0V8FsW07/niLn3S0kKBg=="
	vBodyKey     = "d95038866247104d7dbb9ce4dd110cc213d8e0d1d5aa91631161f43ad54f89ed"
)

// seedSigner is a Signer with a fixed ed25519 seed.
type seedSigner struct{ priv ed25519.PrivateKey }

func newSeedSigner(b byte) seedSigner {
	return seedSigner{priv: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, ed25519.SeedSize))}
}

func (s seedSigner) pub() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }
func (s seedSigner) Fingerprint() string    { return node.FingerprintOf(s.pub()) }
func (s seedSigner) PublicKeyB64() string   { return base64.StdEncoding.EncodeToString(s.pub()) }
func (s seedSigner) Sign(msg []byte) []byte {
	return ed25519.Sign(s.priv, msg)
}

func vectorOffer(t *testing.T) (*Offer, seedSigner, seedSigner) {
	t.Helper()
	snd, rcv := newSeedSigner(1), newSeedSigner(2)
	if snd.PublicKeyB64() != vSenderPub || snd.Fingerprint() != vSenderFP || rcv.PublicKeyB64() != vReceiverPub || rcv.Fingerprint() != vReceiverFP {
		t.Fatal("seed keys do not match the vectors")
	}
	o := &Offer{
		Proto: Version, SenderName: "laptop", SenderFP: vSenderFP, SenderPub: vSenderPub,
		ReceiverFP: vReceiverFP, TS: vTS, Nonce: vNonce, Eph: vEphA,
		Manifest: Manifest{Kind: KindMulti, Entries: []Entry{
			{Path: "photos", Mode: ModeDir},
			{Path: "photos/beach.jpg", Size: 5, Mode: 0o644},
		}, Total: 5},
	}
	return o, snd, rcv
}

func x25519Key(t *testing.T, h string) *ecdh.PrivateKey {
	t.Helper()
	b, _ := hex.DecodeString(h)
	k, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestOfferSigningVector(t *testing.T) {
	o, snd, rcv := vectorOffer(t)
	cj, _ := o.Manifest.CanonicalJSON()
	if string(cj) != vManifest {
		t.Fatalf("canonical manifest %s", cj)
	}
	if h, _ := o.Manifest.Hash(); h != vManifestH {
		t.Fatalf("manifest hash %s", h)
	}
	sb, err := SigningBytes(o)
	if err != nil || string(sb) != vOfferSB {
		t.Fatalf("offer signing bytes:\n%q\nwant\n%q", sb, vOfferSB)
	}
	if err := o.SignWith(snd); err != nil || o.Sig != vOfferSig {
		t.Fatalf("offer signature %s", o.Sig)
	}
	v := NewVerifier(rcv.Fingerprint())
	v.Now = func() time.Time { return time.Unix(vTS, 0) }
	if _, err := v.Verify(o); err != nil {
		t.Fatalf("vector offer rejected: %v", err)
	}
}

func TestReplySigningVectors(t *testing.T) {
	o, _, rcv := vectorOffer(t)
	sb, _ := SigningBytes(o)
	cases := []struct {
		name    string
		r       OfferReply
		sb, sig string
	}{
		{"accepted", OfferReply{Accepted: true, TransferID: vTransferID, Token: vToken, Eph: vEphB}, vReplySB, vReplySig},
		{"resume", OfferReply{Accepted: true, TransferID: vTransferID, Token: vToken, Eph: vEphB,
			Resume: &ResumePoint{Entry: 1, Offset: 1 << 20, SHA256: strings.Repeat("ab", 32)}}, vResumeSB, vResumeSig},
		{"declined", OfferReply{TransferID: vTransferID, Reason: ReasonDeclined}, vDeclineSB, vDeclineSig},
	}
	for _, c := range cases {
		if got := ReplySigningBytes(sb, &c.r); string(got) != c.sb {
			t.Errorf("%s: signing bytes\n%q\nwant\n%q", c.name, got, c.sb)
		}
		SignReply(rcv, sb, &c.r)
		if c.r.Sig != c.sig {
			t.Errorf("%s: signature %s", c.name, c.r.Sig)
		}
		if err := VerifyReply(rcv.pub(), sb, &c.r); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestBodyKeyVector(t *testing.T) {
	o, _, _ := vectorOffer(t)
	a, b := x25519Key(t, vEphAPriv), x25519Key(t, vEphBPriv)
	ks, err := BodyKey(a, vEphB, o, vTransferID) // sender: own offer key, reply eph
	if err != nil {
		t.Fatal(err)
	}
	kr, err := BodyKey(b, o.Eph, o, vTransferID) // receiver: own reply key, offer eph
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(ks) != vBodyKey || hex.EncodeToString(kr) != vBodyKey {
		t.Fatalf("body keys %x / %x", ks, kr)
	}
	if _, err := BodyKey(a, "bad", o, vTransferID); !errors.Is(err, ErrEph) {
		t.Fatalf("bad peer eph: %v", err)
	}
}

func TestVerifyReplyRejects(t *testing.T) {
	o, snd, rcv := vectorOffer(t)
	sb, _ := SigningBytes(o)
	good := func() *OfferReply {
		r := &OfferReply{Accepted: true, TransferID: vTransferID, Token: vToken, Eph: vEphB}
		SignReply(rcv, sb, r)
		return r
	}
	if err := VerifyReply(rcv.pub(), sb, good()); err != nil {
		t.Fatal(err)
	}
	other, _ := SigningBytes(&Offer{Nonce: "x", Manifest: o.Manifest})
	mutations := map[string]func(r *OfferReply) ([]byte, ed25519.PublicKey){
		"unsigned":         func(r *OfferReply) ([]byte, ed25519.PublicKey) { r.Sig = ""; return sb, rcv.pub() },
		"not base64":       func(r *OfferReply) ([]byte, ed25519.PublicKey) { r.Sig = "!!"; return sb, rcv.pub() },
		"signed by sender": func(r *OfferReply) ([]byte, ed25519.PublicKey) { SignReply(snd, sb, r); return sb, rcv.pub() },
		"other offer":      func(r *OfferReply) ([]byte, ed25519.PublicKey) { return other, rcv.pub() },
		"accepted flipped": func(r *OfferReply) ([]byte, ed25519.PublicKey) { r.Accepted = false; return sb, rcv.pub() },
		"token changed": func(r *OfferReply) ([]byte, ed25519.PublicKey) {
			r.Token = strings.Repeat("0", 32)
			return sb, rcv.pub()
		},
		"transfer changed": func(r *OfferReply) ([]byte, ed25519.PublicKey) {
			r.TransferID = strings.Repeat("1", 32)
			return sb, rcv.pub()
		},
		"eph changed": func(r *OfferReply) ([]byte, ed25519.PublicKey) { r.Eph = vEphA; return sb, rcv.pub() },
		"resume added": func(r *OfferReply) ([]byte, ed25519.PublicKey) {
			r.Resume = &ResumePoint{Entry: 1, SHA256: strings.Repeat("0", 64)}
			return sb, rcv.pub()
		},
		"reason added": func(r *OfferReply) ([]byte, ed25519.PublicKey) { r.Reason = "x"; return sb, rcv.pub() },
		"newline reason": func(r *OfferReply) ([]byte, ed25519.PublicKey) {
			r.Reason = "a\nb"
			SignReply(rcv, sb, r)
			return sb, rcv.pub()
		},
		"newline token": func(r *OfferReply) ([]byte, ed25519.PublicKey) {
			r.Token = vToken + "\n" + vToken
			SignReply(rcv, sb, r)
			return sb, rcv.pub()
		},
		"accepted without eph": func(r *OfferReply) ([]byte, ed25519.PublicKey) {
			r.Eph = ""
			SignReply(rcv, sb, r)
			return sb, rcv.pub()
		},
		"accepted without token": func(r *OfferReply) ([]byte, ed25519.PublicKey) {
			r.Token = ""
			SignReply(rcv, sb, r)
			return sb, rcv.pub()
		},
		"bad resume hash": func(r *OfferReply) ([]byte, ed25519.PublicKey) {
			r.Resume = &ResumePoint{SHA256: "AB"}
			SignReply(rcv, sb, r)
			return sb, rcv.pub()
		},
		"non-canonical eph": func(r *OfferReply) ([]byte, ed25519.PublicKey) {
			r.Eph = strings.TrimRight(vEphB, "=")
			SignReply(rcv, sb, r)
			return sb, rcv.pub()
		},
		"wrong key length": func(r *OfferReply) ([]byte, ed25519.PublicKey) { return sb, rcv.pub()[:31] },
	}
	for name, mut := range mutations {
		r := good()
		osb, pub := mut(r)
		if err := VerifyReply(pub, osb, r); !errors.Is(err, ErrReply) {
			t.Errorf("%s: %v, want ErrReply", name, err)
		}
	}
}

func TestVerifyEph(t *testing.T) {
	snd, rcv, v, _ := fixture(t)
	for name, eph := range map[string]string{
		"missing":       "",
		"not base64":    "!!!",
		"short":         base64.StdEncoding.EncodeToString(make([]byte, 31)),
		"long":          base64.StdEncoding.EncodeToString(make([]byte, 33)),
		"unpadded":      strings.TrimRight(vEphA, "="),
		"url alphabet":  base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32)),
		"newline added": vEphA + "\n",
	} {
		o, _, _ := NewOffer(snd, snd.Name, rcv.Fingerprint(), FileManifest("a", 1, 0), t0)
		o.Eph = eph
		_ = o.SignWith(snd)
		if _, err := v.Verify(o); !errors.Is(err, ErrEph) {
			t.Errorf("eph %s: %v, want ErrEph", name, err)
		}
	}
	// The eph is signed: swapping it after signing breaks the signature.
	o, _, _ := NewOffer(snd, snd.Name, rcv.Fingerprint(), FileManifest("a", 1, 0), t0)
	o.Eph = vEphA
	if _, err := v.Verify(o); !errors.Is(err, ErrSignature) {
		t.Fatalf("swapped eph: %v, want ErrSignature", err)
	}
	// Each offer gets a fresh key.
	o2, k2, _ := NewOffer(snd, snd.Name, rcv.Fingerprint(), FileManifest("a", 1, 0), t0)
	o3, k3, _ := NewOffer(snd, snd.Name, rcv.Fingerprint(), FileManifest("a", 1, 0), t0)
	if o2.Eph == o3.Eph || k2.Equal(k3) {
		t.Fatal("ephemeral key reused across offers")
	}
	if base64.StdEncoding.EncodeToString(k2.PublicKey().Bytes()) != o2.Eph {
		t.Fatal("returned key does not match the offer's eph")
	}
}

func TestDisplayName(t *testing.T) {
	for in, want := range map[string]string{
		"desk":                  "desk",
		"  my \t desk ":         "my desk",
		"‮evil​\x07":            "evil",
		"":                      "fallback",
		strings.Repeat("é", 70): strings.Repeat("é", 64),
		"\x00\x01":              "fallback",
	} {
		if got := DisplayName(in, "fallback"); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}
