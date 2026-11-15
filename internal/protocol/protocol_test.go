package protocol

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/node"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func newID(t *testing.T, name string) *node.Identity {
	t.Helper()
	id, err := node.Load(t.TempDir(), name)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// fixture returns a sender, a receiver verifier pinned at t0 and a valid offer.
func fixture(t *testing.T) (*node.Identity, *node.Identity, *Verifier, *Offer) {
	t.Helper()
	snd, rcv := newID(t, "alice"), newID(t, "bob")
	v := NewVerifier(rcv.Fingerprint())
	v.Now = func() time.Time { return t0 }
	o, _, err := NewOffer(snd, snd.Name, rcv.Fingerprint(), FileManifest("report.pdf", 1234, 0o644), t0)
	if err != nil {
		t.Fatal(err)
	}
	return snd, rcv, v, o
}

func TestVerifyGoodOffer(t *testing.T) {
	snd, _, v, o := fixture(t)
	pub, err := v.Verify(o)
	if err != nil {
		t.Fatalf("valid offer rejected: %v", err)
	}
	if node.FingerprintOf(pub) != snd.Fingerprint() {
		t.Fatal("returned key is not the sender's")
	}
}

func TestVerifyForgedFingerprint(t *testing.T) {
	_, _, v, o := fixture(t)
	// Claim someone else's fingerprint but present our own key (and re-sign,
	// so only the fingerprint/key binding is wrong).
	victim := newID(t, "victim")
	o.SenderFP = victim.Fingerprint()
	if _, err := v.Verify(o); !errors.Is(err, ErrFingerprint) {
		t.Fatalf("forged fingerprint: got %v, want ErrFingerprint", err)
	}
}

func TestVerifyForgedKeyWithoutPrivateKey(t *testing.T) {
	_, _, v, o := fixture(t)
	// Present the victim's real key and fingerprint, but the attacker cannot
	// sign with the victim's private key: the signature must fail.
	victim := newID(t, "victim")
	o.SenderFP = victim.Fingerprint()
	o.SenderPub = victim.PublicKeyB64()
	if _, err := v.Verify(o); !errors.Is(err, ErrSignature) {
		t.Fatalf("impersonation: got %v, want ErrSignature", err)
	}
}

func TestVerifyWrongReceiver(t *testing.T) {
	snd, _, v, _ := fixture(t)
	other := newID(t, "carol")
	o, _, err := NewOffer(snd, snd.Name, other.Fingerprint(), FileManifest("a.txt", 1, 0o644), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(o); !errors.Is(err, ErrWrongReceiver) {
		t.Fatalf("wrong receiver: got %v, want ErrWrongReceiver", err)
	}
}

func TestVerifyStale(t *testing.T) {
	for _, d := range []time.Duration{-6 * time.Minute, 6 * time.Minute, -24 * time.Hour} {
		snd, rcv, v, _ := fixture(t)
		o, _, err := NewOffer(snd, snd.Name, rcv.Fingerprint(), FileManifest("a.txt", 1, 0o644), t0.Add(d))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(o); !errors.Is(err, ErrStale) {
			t.Fatalf("ts off by %s: got %v, want ErrStale", d, err)
		}
	}
	// Inside the window is fine.
	snd, rcv, v, _ := fixture(t)
	o, _, _ := NewOffer(snd, snd.Name, rcv.Fingerprint(), FileManifest("a.txt", 1, 0o644), t0.Add(-4*time.Minute))
	if _, err := v.Verify(o); err != nil {
		t.Fatalf("ts 4m old rejected: %v", err)
	}
}

func TestVerifyReplay(t *testing.T) {
	_, _, v, o := fixture(t)
	if _, err := v.Verify(o); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(o); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay: got %v, want ErrReplay", err)
	}
}

func TestBadSignatureDoesNotBurnNonce(t *testing.T) {
	snd, _, v, o := fixture(t)
	good := *o
	o.Sig = good.Sig[:len(good.Sig)-4] + "AAA="
	if _, err := v.Verify(o); !errors.Is(err, ErrSignature) {
		t.Fatalf("bad sig: got %v", err)
	}
	if _, err := v.Verify(&good); err != nil {
		t.Fatalf("good offer after a forged one with the same nonce: %v", err)
	}
	_ = snd
}

func TestVerifyTamperedManifest(t *testing.T) {
	_, _, v, o := fixture(t)
	o.Manifest = FileManifest("report.pdf", 999999, 0o644)
	if _, err := v.Verify(o); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered manifest: got %v, want ErrSignature", err)
	}
}

func TestVerifyProtoAndNonce(t *testing.T) {
	_, _, v, o := fixture(t)
	for _, p := range []int{0, 1, 3} {
		o.Proto = p
		if _, err := v.Verify(o); !errors.Is(err, ErrProto) {
			t.Fatalf("proto %d: got %v", p, err)
		}
	}
	snd, rcv, v, _ := fixture(t)
	o, _, _ = NewOffer(snd, snd.Name, rcv.Fingerprint(), FileManifest("a", 1, 0), t0)
	o.Nonce = "short"
	_ = o.SignWith(snd)
	if _, err := v.Verify(o); !errors.Is(err, ErrNonce) {
		t.Fatalf("short nonce: got %v", err)
	}
	o.Nonce = "aaaaaaaaaaaaaaaa\nbbbb"
	_ = o.SignWith(snd)
	if _, err := v.Verify(o); !errors.Is(err, ErrNonce) {
		t.Fatalf("newline nonce: got %v", err)
	}
	o.Nonce = NewNonce()
	o.SenderPub = "not base64!"
	if _, err := v.Verify(o); !errors.Is(err, ErrPublicKey) {
		t.Fatalf("bad pub: got %v", err)
	}
}

func TestSigningBytesFormat(t *testing.T) {
	o := &Offer{ReceiverFP: "rrrr", SenderFP: "ssss", TS: 1700000000, Nonce: "nnnn", Eph: "eeee",
		Manifest: FileManifest("a.txt", 3, 0o644)}
	b, err := SigningBytes(o)
	if err != nil {
		t.Fatal(err)
	}
	mh, _ := o.Manifest.Hash()
	want := "rocket-offer-v2\nrrrr\nssss\n1700000000\nnnnn\neeee\n" + mh
	if string(b) != want {
		t.Fatalf("signing bytes:\n%q\nwant\n%q", b, want)
	}
	cj, _ := o.Manifest.CanonicalJSON()
	if string(cj) != `{"kind":"file","entries":[{"path":"a.txt","size":3,"mode":420}],"total":3}` {
		t.Fatalf("canonical JSON: %s", cj)
	}
	// No HTML escaping: the canonical form is the literal bytes.
	cj, _ = FileManifest("a<b>&c", 0, 0).CanonicalJSON()
	if !strings.Contains(string(cj), `"a<b>&c"`) {
		t.Fatalf("canonical JSON must not HTML-escape: %s", cj)
	}
}

func TestCheckHello(t *testing.T) {
	id := newID(t, "bob")
	h := Hello{Proto: 2, Name: "bob", FP: id.Fingerprint(), Pub: id.PublicKeyB64()}
	if _, err := CheckHello(h, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckHello(h, id.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckHello(h, "0000000000000000"); err == nil {
		t.Fatal("hello from an unexpected fingerprint accepted")
	}
	h.FP = "0000000000000000"
	if _, err := CheckHello(h, ""); !errors.Is(err, ErrFingerprint) {
		t.Fatalf("forged hello fp: %v", err)
	}
	// A 1.0 peer (protocol 1) is refused with the update message.
	h.FP, h.Proto = id.Fingerprint(), 1
	_, err := CheckHello(h, "")
	if !errors.Is(err, ErrProto) || err.Error() != "bob runs an older rocket (protocol 1); update it to 1.1 or later" {
		t.Fatalf("hello proto 1: %v", err)
	}
	h.Proto, h.Name = 0, "‮mallory\n"
	if _, err := CheckHello(h, ""); err == nil || err.Error() != "mallory runs an older rocket (protocol 1); update it to 1.1 or later" {
		t.Fatalf("hello without proto: %v", err)
	}
	h.Proto, h.Name = 3, ""
	if _, err := CheckHello(h, ""); !errors.Is(err, ErrProto) || !strings.Contains(err.Error(), "newer rocket (protocol 3)") {
		t.Fatalf("hello proto 3: %v", err)
	}
}

func TestValidatePathTraversal(t *testing.T) {
	bad := []string{
		"", "..", "../etc/passwd", "a/../../b", "a/..", "./a", "a/./b", "a//b", "a/",
		"/etc/passwd", "//server/share/x", `\\server\share\x`, `C:\Windows\x`, "C:x", "C:/x",
		`..\..\x`, `a\b`, "a\x00b", "a\nb", "file.txt:stream", ".",
		strings.Repeat("a", 256), strings.Repeat("a/", 2100) + "b", "\xff\xfe",
	}
	for _, p := range bad {
		if err := ValidatePath(p); err == nil {
			t.Errorf("ValidatePath(%q) accepted a bad path", p)
		}
	}
	good := []string{"a", "a.txt", "dir/sub/file", "unicode/ファイル.txt", ".hidden", "a b/c d", "..a", "a..b"}
	for _, p := range good {
		if err := ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) rejected a good path: %v", p, err)
		}
	}
}

func TestManifestValidate(t *testing.T) {
	ok := []Manifest{
		FileManifest("a.txt", 10, 0o644),
		FileManifest("empty", 0, 0o600),
		{Kind: KindMulti, Total: 3, Entries: []Entry{
			{Path: "d", Mode: ModeDir | 0o755}, {Path: "d/a", Size: 1}, {Path: "d/b", Size: 2}, {Path: "d/e", Mode: ModeDir}}},
	}
	for i, m := range ok {
		if err := m.Validate(); err != nil {
			t.Errorf("ok[%d] rejected: %v", i, err)
		}
	}
	many := make([]Entry, MaxEntries+1)
	for i := range many {
		many[i] = Entry{Path: "f" + strings.Repeat("x", i%7) + "/" + itoa(i)}
	}
	bad := map[string]Manifest{
		"unknown kind":      {Kind: "zip", Entries: []Entry{{Path: "a"}}},
		"file two entries":  {Kind: KindFile, Entries: []Entry{{Path: "a"}, {Path: "b"}}},
		"file nested":       {Kind: KindFile, Entries: []Entry{{Path: "d/a", Size: 1}}, Total: 1},
		"file dir":          {Kind: KindFile, Entries: []Entry{{Path: "d", Mode: ModeDir}}},
		"multi empty":       {Kind: KindMulti},
		"traversal":         {Kind: KindMulti, Entries: []Entry{{Path: "../x", Size: 1}}, Total: 1},
		"file traversal":    {Kind: KindFile, Entries: []Entry{{Path: "..", Size: 1}}, Total: 1},
		"absolute":          {Kind: KindMulti, Entries: []Entry{{Path: "/x", Size: 1}}, Total: 1},
		"volume":            {Kind: KindMulti, Entries: []Entry{{Path: "C:/x", Size: 1}}, Total: 1},
		"unc":               {Kind: KindMulti, Entries: []Entry{{Path: `\\srv\share`, Size: 1}}, Total: 1},
		"nul":               {Kind: KindMulti, Entries: []Entry{{Path: "a\x00", Size: 1}}, Total: 1},
		"total mismatch":    FileManifest("a", 5, 0), /* fixed below */
		"negative":          {Kind: KindFile, Entries: []Entry{{Path: "a", Size: -1}}, Total: -1},
		"too big":           {Kind: KindFile, Entries: []Entry{{Path: "a", Size: MaxTotal + 1}}, Total: MaxTotal + 1},
		"dup":               {Kind: KindMulti, Entries: []Entry{{Path: "a/b", Size: 1}, {Path: "A/B", Size: 1}}, Total: 2},
		"file as dir":       {Kind: KindMulti, Entries: []Entry{{Path: "a", Size: 1}, {Path: "a/b", Size: 1}}, Total: 2},
		"dir with size":     {Kind: KindMulti, Entries: []Entry{{Path: "a", Size: 1, Mode: ModeDir}}, Total: 1},
		"setuid mode":       {Kind: KindFile, Entries: []Entry{{Path: "a", Mode: 0o4755}}},
		"too many entries":  {Kind: KindMulti, Entries: many},
		"sum overflow-ish":  {Kind: KindMulti, Entries: []Entry{{Path: "a", Size: MaxTotal}, {Path: "b", Size: 1}}, Total: MaxTotal + 1},
		"multi total wrong": {Kind: KindMulti, Entries: []Entry{{Path: "a", Size: 1}}, Total: 2},
	}
	m := bad["total mismatch"]
	m.Total = 4
	bad["total mismatch"] = m
	for name, m := range bad {
		if err := m.Validate(); !errors.Is(err, ErrManifest) {
			t.Errorf("%s: got %v, want ErrManifest", name, err)
		}
	}
}

func TestOfferWithTraversalRejected(t *testing.T) {
	snd, rcv, v, _ := fixture(t)
	m := Manifest{Kind: KindMulti, Entries: []Entry{{Path: "../../evil", Size: 1}}, Total: 1}
	o, _, err := NewOffer(snd, snd.Name, rcv.Fingerprint(), m, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(o); !errors.Is(err, ErrManifest) {
		t.Fatalf("traversal offer: got %v, want ErrManifest", err)
	}
}

// TestNonceCacheNeverEvictsFreshNonces: a full cache refuses new nonces
// rather than forgetting one that could still pass the freshness check, so
// flooding it cannot make a captured offer replayable.
func TestNonceCacheNeverEvictsFreshNonces(t *testing.T) {
	c := NewNonceCache(3)
	for _, k := range []string{"a", "b", "c"} {
		if err := c.Use(k, t0); err != nil {
			t.Fatalf("first use of %s: %v", k, err)
		}
	}
	if err := c.Use("a", t0); !errors.Is(err, ErrReplay) {
		t.Fatalf("reuse of a: %v, want ErrReplay", err)
	}
	if err := c.Use("d", t0.Add(time.Minute)); !errors.Is(err, ErrBusy) {
		t.Fatalf("use while full of fresh nonces: %v, want ErrBusy", err)
	}
	if c.Len() != 3 {
		t.Fatalf("len %d, want 3", c.Len())
	}
	// Just inside the window, every nonce is still remembered.
	edge := t0.Add(2 * MaxClockSkew)
	if err := c.Use("a", edge); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay at the window edge: %v, want ErrReplay", err)
	}
	// Entries older than 2x the skew window are forgotten, making room.
	later := edge.Add(time.Second)
	if err := c.Use("d", later); err != nil {
		t.Fatalf("use after the window: %v", err)
	}
	if c.Len() != 1 {
		t.Fatalf("len %d after expiry, want 1", c.Len())
	}
}

// TestVerifyFloodCannotFlushNonce replays the attack: capture a good offer,
// fill the cache with offers from throwaway keys, then replay. The replay
// must still be refused.
func TestVerifyFloodCannotFlushNonce(t *testing.T) {
	_, _, v, captured := fixture(t)
	v.nonces = NewNonceCache(8)
	recvFP := captured.ReceiverFP
	m := FileManifest("x.bin", 1, 0o644)
	if _, err := v.Verify(captured); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	busy := false
	for i := 0; i < 20; i++ {
		o, _, err := NewOffer(newID(t, "flood"), "flood", recvFP, m, t0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(o); errors.Is(err, ErrBusy) {
			busy = true
		} else if err != nil {
			t.Fatalf("flood offer %d: %v", i, err)
		}
	}
	if !busy {
		t.Fatal("a full cache never answered ErrBusy")
	}
	if _, err := v.Verify(captured); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay after a flood: %v, want ErrReplay", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
