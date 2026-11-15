package securestream

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"testing"
	"testing/iotest"
)

// The vectors below were computed independently of this package, with
// Node.js crypto (X25519, HKDF-SHA256, ChaCha20-Poly1305). The X25519 keys
// and shared secret are the ones of RFC 7748, section 6.1.
const (
	vAlicePriv = "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a"
	vAlicePub  = "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a"
	vBobPriv   = "5dab087e624a8a4b79e17f8b83800ee66f3bb1292618b6fd1c2f8b27ff88e0eb"
	vBobPub    = "de9edb7d7b7dc1b4d35b61c2ece435373f8343c85b78674dadfc7e146f882b4f"
	vShared    = "4a5d9d5ba4ce2de1728e3bf480350f25e07e21c947d19e3376f09b3c1e161742"

	// The offer signing bytes the salt is hashed from (see the protocol
	// package's vector test, which builds them from an offer).
	vOfferSB = "rocket-offer-v2\n6a3803d5f059902a\n34750f98bd59fcfc\n1794657600\n3f7a9c2e4b1d8a6f0e5c7b9a2d4f6e8c\n" +
		"hSDwCYkwp1R0i33ctD73Wg2/Og0mOBr066SpjqqbTmo=\ne134e1fa49cbdca1e8f8a167f8374acc503e894760b40ab8b6e3b6b8ed81477e"
	vSalt       = "561837af2b824379813ec41ef8e019d57f4a5a1be5165271aef733f3ea4b0e16"
	vSenderFP   = "34750f98bd59fcfc"
	vReceiverFP = "6a3803d5f059902a"
	vTransferID = "00112233445566778899aabbccddeeff"
	vKey        = "d95038866247104d7dbb9ce4dd110cc213d8e0d1d5aa91631161f43ad54f89ed"

	// Streams under vKey. small: the 16-byte start header (0,0) and "hello"
	// in one final frame. empty: one empty final frame.
	vSmall = "000000251f9ce57c0ef08e5fd2dbb96f24c7cd1f480b167c6e5789c19c370c1dbcbf997ece2bab6d77"
	vEmpty = "0000001032f4eaccaa38e97eb121b7434e563f93"
	// big: 2*65536+100 bytes of i%251, as two full frames and a final one.
	vBigSHA256 = "e1317af8830c718e81f24bab85235813a8ad54e2e9474f18768eb902697c31d5"
	vBigLen    = 131232
	// full: the first 65536 bytes of big, as one full final frame.
	vFullSHA256 = "a4c9fe72c611c521bd63b352a35685162c176162b40d3f1e8f7d5063f60a0bd1"
	vFullLen    = 65556
)

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func x25519(t testing.TB, privHex string) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().NewPrivateKey(unhex(t, privHex))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func vectorKey(t testing.TB) []byte { return unhex(t, vKey) }

func smallPlain() []byte { return append(make([]byte, 16), "hello"...) }

func bigPlain() []byte {
	b := make([]byte, 2*MaxPlain+100)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestDeriveKeyVector(t *testing.T) {
	alice, bob := x25519(t, vAlicePriv), x25519(t, vBobPriv)
	if hex.EncodeToString(alice.PublicKey().Bytes()) != vAlicePub || hex.EncodeToString(bob.PublicKey().Bytes()) != vBobPub {
		t.Fatal("RFC 7748 public keys do not match")
	}
	shared, err := alice.ECDH(bob.PublicKey())
	if err != nil || hex.EncodeToString(shared) != vShared {
		t.Fatalf("shared secret %x (%v)", shared, err)
	}
	if s := sha256.Sum256([]byte(vOfferSB)); hex.EncodeToString(s[:]) != vSalt {
		t.Fatalf("salt %x", s)
	}
	if got := KeyInfo(vSenderFP, vReceiverFP, vTransferID); got != "rocket-v2 body\n34750f98bd59fcfc\n6a3803d5f059902a\n00112233445566778899aabbccddeeff" {
		t.Fatalf("info %q", got)
	}
	// Both sides derive the same key.
	k1, err := DeriveKey(alice, bob.PublicKey().Bytes(), []byte(vOfferSB), vSenderFP, vReceiverFP, vTransferID)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := DeriveKey(bob, alice.PublicKey().Bytes(), []byte(vOfferSB), vSenderFP, vReceiverFP, vTransferID)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(k1) != vKey || hex.EncodeToString(k2) != vKey {
		t.Fatalf("keys %x / %x, want %s", k1, k2, vKey)
	}
	// Every input changes the key.
	for name, f := range map[string]func() ([]byte, error){
		"offer": func() ([]byte, error) {
			return DeriveKey(alice, bob.PublicKey().Bytes(), []byte(vOfferSB+"x"), vSenderFP, vReceiverFP, vTransferID)
		},
		"sender": func() ([]byte, error) {
			return DeriveKey(alice, bob.PublicKey().Bytes(), []byte(vOfferSB), vReceiverFP, vReceiverFP, vTransferID)
		},
		"receiver": func() ([]byte, error) {
			return DeriveKey(alice, bob.PublicKey().Bytes(), []byte(vOfferSB), vSenderFP, vSenderFP, vTransferID)
		},
		"transfer": func() ([]byte, error) {
			return DeriveKey(alice, bob.PublicKey().Bytes(), []byte(vOfferSB), vSenderFP, vReceiverFP, "x"+vTransferID[1:])
		},
	} {
		k, err := f()
		if err != nil || bytes.Equal(k, k1) {
			t.Errorf("%s: key unchanged (%v)", name, err)
		}
	}
}

func TestDeriveKeyRejectsLowOrderPoints(t *testing.T) {
	alice := x25519(t, vAlicePriv)
	// The all-zero point and other small-order points give an all-zero
	// shared secret.
	for _, p := range []string{
		"0000000000000000000000000000000000000000000000000000000000000000",
		"0100000000000000000000000000000000000000000000000000000000000000",
		"e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800",
	} {
		if _, err := DeriveKey(alice, unhex(t, p), []byte(vOfferSB), vSenderFP, vReceiverFP, vTransferID); err == nil {
			t.Errorf("low-order point %s accepted", p)
		}
	}
	if _, err := DeriveKey(alice, []byte("short"), nil, "", "", ""); err == nil {
		t.Error("short peer key accepted")
	}
	if _, err := DeriveKey(nil, alice.PublicKey().Bytes(), nil, "", "", ""); err == nil {
		t.Error("nil private key accepted")
	}
	if allZero([]byte{0, 0, 1}) || !allZero(make([]byte, 32)) {
		t.Fatal("allZero")
	}
}

func TestStreamVectors(t *testing.T) {
	key := vectorKey(t)
	cases := []struct {
		name   string
		plain  []byte
		stream func(got []byte) bool
	}{
		{"small", smallPlain(), func(got []byte) bool { return hex.EncodeToString(got) == vSmall }},
		{"empty", nil, func(got []byte) bool { return hex.EncodeToString(got) == vEmpty }},
		{"big", bigPlain(), func(got []byte) bool { return len(got) == vBigLen && sum(got) == vBigSHA256 }},
		{"full", bigPlain()[:MaxPlain], func(got []byte) bool { return len(got) == vFullLen && sum(got) == vFullSHA256 }},
	}
	for _, c := range cases {
		got, err := Seal(key, c.plain)
		if err != nil {
			t.Fatal(err)
		}
		if !c.stream(got) {
			t.Errorf("%s: stream does not match the vector (%d bytes, sha256 %s)", c.name, len(got), sum(got))
		}
		if int64(len(got)) != SealedSize(int64(len(c.plain))) {
			t.Errorf("%s: SealedSize %d, stream %d", c.name, SealedSize(int64(len(c.plain))), len(got))
		}
		back, err := open(key, got)
		if err != nil || !bytes.Equal(back, c.plain) {
			t.Errorf("%s: decrypt: %v (%d bytes)", c.name, err, len(back))
		}
	}
}

func open(key, stream []byte) ([]byte, error) {
	r, err := NewReader(bytes.NewReader(stream), key)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// frames splits a stream into its frames (length prefix included).
func frames(t *testing.T, stream []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(stream) > 0 {
		n := int(binary.BigEndian.Uint32(stream)) + headerSize
		out = append(out, stream[:n])
		stream = stream[n:]
	}
	return out
}

func join(fs ...[]byte) []byte { return bytes.Join(fs, nil) }

func TestRoundTrip(t *testing.T) {
	key := make([]byte, KeySize)
	_, _ = rand.Read(key)
	for _, n := range []int{0, 1, 15, 16, MaxPlain - 1, MaxPlain, MaxPlain + 1, 3 * MaxPlain, 3*MaxPlain + 7} {
		plain := make([]byte, n)
		_, _ = rand.Read(plain)
		var buf bytes.Buffer
		w, err := NewWriter(&buf, key)
		if err != nil {
			t.Fatal(err)
		}
		// Uneven writes.
		for rest, k := plain, 1; len(rest) > 0; k = k*3 + 1 {
			k = min(k, len(rest))
			if m, err := w.Write(rest[:k]); err != nil || m != k {
				t.Fatalf("write: %d %v", m, err)
			}
			rest = rest[k:]
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil { // idempotent
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); !errors.Is(err, ErrClosed) {
			t.Fatalf("write after close: %v", err)
		}
		if int64(buf.Len()) != SealedSize(int64(n)) {
			t.Fatalf("%d bytes: stream %d, SealedSize %d", n, buf.Len(), SealedSize(int64(n)))
		}
		for _, f := range frames(t, buf.Bytes()) {
			if len(f)-headerSize > MaxCipher {
				t.Fatalf("frame of %d bytes", len(f))
			}
		}
		// Byte-at-a-time and half reads see the same plaintext.
		for name, wrap := range map[string]func(io.Reader) io.Reader{
			"whole": func(r io.Reader) io.Reader { return r },
			"one":   iotest.OneByteReader,
			"half":  iotest.HalfReader,
		} {
			r, _ := NewReader(wrap(bytes.NewReader(buf.Bytes())), key)
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("%d bytes, %s reader: %v", n, name, err)
			}
		}
	}
}

func TestWrongKey(t *testing.T) {
	other := vectorKey(t)
	other[0] ^= 1
	if _, err := open(other, unhex(t, vSmall)); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := NewReader(nil, []byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := NewWriter(nil, []byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestTamper(t *testing.T) {
	key := vectorKey(t)
	small := unhex(t, vSmall)
	for i := range small {
		for _, bit := range []byte{0x01, 0x80} {
			s := bytes.Clone(small)
			s[i] ^= bit
			out, err := open(key, s)
			if err == nil {
				t.Fatalf("byte %d bit %#x: tampered stream accepted", i, bit)
			}
			if len(out) != 0 {
				t.Fatalf("byte %d: %d plaintext bytes released from a tampered frame", i, len(out))
			}
			if !errors.Is(err, ErrAuth) && !errors.Is(err, ErrFrameSize) && !errors.Is(err, ErrTruncated) {
				t.Fatalf("byte %d: unexpected error %v", i, err)
			}
		}
	}
	// A flipped bit in a later frame of a long stream: the earlier frames are
	// released, the bad one is not, and the stream fails.
	big, _ := Seal(key, bigPlain())
	fs := frames(t, big)
	fs[1] = bytes.Clone(fs[1])
	fs[1][100] ^= 1
	r, _ := NewReader(bytes.NewReader(join(fs...)), key)
	got, err := io.ReadAll(r)
	if !errors.Is(err, ErrAuth) || len(got) != MaxPlain {
		t.Fatalf("tampered frame 1: %v after %d bytes", err, len(got))
	}
	// Errors stick.
	if n, err2 := r.Read(make([]byte, 10)); n != 0 || err2 != err {
		t.Fatalf("read after error: %d %v", n, err2)
	}
}

func TestTruncate(t *testing.T) {
	key := vectorKey(t)
	small := unhex(t, vSmall)
	for n := 0; n < len(small); n++ {
		out, err := open(key, small[:n])
		if !errors.Is(err, ErrTruncated) || !errors.Is(err, io.ErrUnexpectedEOF) || len(out) != 0 {
			t.Fatalf("cut at %d: %v (%d bytes)", n, err, len(out))
		}
	}
	// Cut at a frame boundary: the final frame never comes.
	big, _ := Seal(key, bigPlain())
	fs := frames(t, big)
	for k := 1; k < len(fs); k++ {
		out, err := open(key, join(fs[:k]...))
		if !errors.Is(err, ErrTruncated) {
			t.Fatalf("%d of %d frames: %v", k, len(fs), err)
		}
		if len(out) != k*MaxPlain {
			t.Fatalf("%d of %d frames: %d bytes released", k, len(fs), len(out))
		}
	}
}

func TestReorderReplayDrop(t *testing.T) {
	key := vectorKey(t)
	big, _ := Seal(key, bigPlain())
	f := frames(t, big)
	cases := map[string][]byte{
		"swap 0 and 1":        join(f[1], f[0], f[2]),
		"replay frame 0":      join(f[0], f[0], f[1], f[2]),
		"replay frame 1":      join(f[0], f[1], f[1], f[2]),
		"drop frame 1":        join(f[0], f[2]),
		"final first":         join(f[2], f[0], f[1]),
		"final twice":         join(f[0], f[1], f[2], f[2]),
		"other stream's head": join(unhex(t, vSmall)[:0], f[0], f[1], frames(t, unhex(t, vSmall))[0]),
	}
	for name, s := range cases {
		_, err := open(key, s)
		switch {
		case err == nil:
			t.Errorf("%s: accepted", name)
		case name == "final twice":
			if !errors.Is(err, ErrTrailingData) {
				t.Errorf("%s: %v, want ErrTrailingData", name, err)
			}
		case !errors.Is(err, ErrAuth):
			t.Errorf("%s: %v, want ErrAuth", name, err)
		}
	}
}

func TestOversizedAndBadFrames(t *testing.T) {
	key := vectorKey(t)
	hdr := func(n uint32) []byte {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], n)
		return b[:]
	}
	for _, n := range []uint32{MaxCipher + 1, math.MaxUint32, 0, Overhead - 1} {
		s := append(hdr(n), make([]byte, 64)...)
		if _, err := open(key, s); !errors.Is(err, ErrFrameSize) {
			t.Errorf("frame of %d: %v", n, err)
		}
	}
	// A frame of exactly MaxCipher is fine (a full frame).
	full, _ := Seal(key, bigPlain()[:MaxPlain])
	if binary.BigEndian.Uint32(full) != MaxCipher {
		t.Fatalf("full frame announces %d", binary.BigEndian.Uint32(full))
	}
	// An empty frame that is not the final one is refused, even with a
	// valid tag.
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, key)
	if err := w.seal(false); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if _, err := open(key, buf.Bytes()); !errors.Is(err, ErrFrameSize) {
		t.Fatalf("empty non-final frame: %v", err)
	}
}

func TestTrailingData(t *testing.T) {
	key := vectorKey(t)
	small := unhex(t, vSmall)
	for name, s := range map[string][]byte{
		"one byte":      append(bytes.Clone(small), 0),
		"second stream": join(small, small),
		"empty frame":   join(small, unhex(t, vEmpty)),
	} {
		r, _ := NewReader(bytes.NewReader(s), key)
		out, err := io.ReadAll(r)
		if !errors.Is(err, ErrTrailingData) {
			t.Errorf("%s: %v", name, err)
		}
		if len(out) != 0 {
			t.Errorf("%s: %d bytes of the final frame released", name, len(out))
		}
	}
}

func TestCounterOverflow(t *testing.T) {
	key := vectorKey(t)
	w, _ := NewWriter(io.Discard, key)
	w.n = math.MaxUint64
	if err := w.Close(); !errors.Is(err, ErrCounter) {
		t.Fatalf("writer at the last counter: %v", err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrCounter) {
		t.Fatalf("writer error is not sticky: %v", err)
	}
	r, _ := NewReader(bytes.NewReader(unhex(t, vSmall)), key)
	r.n = math.MaxUint64
	if _, err := io.ReadAll(r); !errors.Is(err, ErrCounter) {
		t.Fatalf("reader at the last counter: %v", err)
	}
	// The frame just before the limit is still usable.
	var buf bytes.Buffer
	w, _ = NewWriter(&buf, key)
	w.n = math.MaxUint64 - 1
	_, _ = w.Write([]byte("edge"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, _ = NewReader(&buf, key)
	r.n = math.MaxUint64 - 1
	if got, err := io.ReadAll(r); err != nil || string(got) != "edge" {
		t.Fatalf("last usable frame: %q %v", got, err)
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n == 0 {
		return 0, io.ErrClosedPipe
	}
	f.n--
	return len(p), nil
}

func TestWriterErrorSticks(t *testing.T) {
	w, _ := NewWriter(&failWriter{n: 1}, vectorKey(t))
	big := bigPlain()
	if _, err := w.Write(big); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("close after error: %v", err)
	}
}

func TestSealedSize(t *testing.T) {
	for plain, want := range map[int64]int64{
		-1: 0, 0: 20, 1: 21, MaxPlain: MaxPlain + 20, MaxPlain + 1: MaxPlain + 1 + 40, 3 * MaxPlain: 3*MaxPlain + 60,
	} {
		if got := SealedSize(plain); got != want {
			t.Errorf("SealedSize(%d) = %d, want %d", plain, got, want)
		}
	}
}

// FuzzReader feeds arbitrary bytes to a Reader. It must never panic or hand
// out more than MaxPlain bytes per frame, and whatever it accepts must be
// exactly what the Writer makes of that plaintext (forging a frame would
// need the key).
func FuzzReader(f *testing.F) {
	key := vectorKey(f)
	f.Add(unhex(f, vSmall))
	f.Add(unhex(f, vEmpty))
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 16})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	two, _ := Seal(key, bytes.Repeat([]byte("ab"), MaxPlain/2+3))
	f.Add(two)
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := NewReader(bytes.NewReader(data), key)
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(r)
		if err != nil {
			if len(out) > len(data) {
				t.Fatalf("%d plaintext bytes from %d input bytes", len(out), len(data))
			}
			return
		}
		again, err := Seal(key, out)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(again, data) {
			t.Fatalf("accepted a stream the writer would not produce")
		}
	})
}
