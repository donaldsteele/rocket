package transfer

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/protocol/securestream"
)

// bodyOf streams a plan through writeBody into memory (plaintext).
func bodyOf(t *testing.T, p *Plan, s Start) []byte {
	t.Helper()
	var buf bytes.Buffer
	err := writeBody(&buf, p.Manifest, s, func(i int, _ protocol.Entry) (io.ReadCloser, error) {
		return os.Open(p.Sources[i])
	}, newTracker(nil, p.Manifest.Total, Files(p.Manifest)))
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBodyRoundTrip(t *testing.T) {
	src := t.TempDir()
	files := map[string]string{"a.txt": "alpha", "sub/b.bin": strings.Repeat("z", 70000), "zero": "", "ü/文件.txt": "unicode"}
	mkTree(t, filepath.Join(src, "proj"), files, "empty/nested")
	p, err := PlanPaths([]string{filepath.Join(src, "proj")})
	if err != nil {
		t.Fatal(err)
	}
	plain := bodyOf(t, p, Start{})
	if int64(len(plain)) != BodySize(p.Manifest, Start{}) || int64(len(plain)) != StartHeaderSize+p.Manifest.Total {
		t.Fatalf("body of %d bytes, BodySize %d", len(plain), BodySize(p.Manifest, Start{}))
	}
	if !bytes.Equal(plain[:StartHeaderSize], make([]byte, StartHeaderSize)) {
		t.Fatalf("start header %x", plain[:StartHeaderSize])
	}
	// Through the encrypted stream, as on the wire.
	key := bytes.Repeat([]byte{7}, securestream.KeySize)
	var wire bytes.Buffer
	if err := writeSealed(&wire, key, p.Manifest, Start{}, func(i int, _ protocol.Entry) (io.ReadCloser, error) {
		return os.Open(p.Sources[i])
	}, newTracker(nil, p.Manifest.Total, Files(p.Manifest))); err != nil {
		t.Fatal(err)
	}
	if int64(wire.Len()) != SealedBodySize(p.Manifest, Start{}) {
		t.Fatalf("sealed body %d bytes, SealedBodySize %d", wire.Len(), SealedBodySize(p.Manifest, Start{}))
	}
	if bytes.Contains(wire.Bytes(), []byte("alpha")) || bytes.Contains(wire.Bytes(), []byte("zzzzzzzz")) {
		t.Fatal("file content visible on the wire")
	}
	sr, _ := securestream.NewReader(bytes.NewReader(wire.Bytes()), key)

	dl := t.TempDir()
	var last Progress
	res, err := ReceiveBody(NewExtractor(dl, p.Manifest, func(pr Progress) { last = pr }), p.Manifest, sr)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 1 || res.Paths[0] != filepath.Join(dl, "proj") || res.Files != 4 || res.Bytes != p.Manifest.Total {
		t.Fatalf("result %+v", res)
	}
	if last.Sent != p.Manifest.Total || last.Files != 4 || last.FileIndex != 4 {
		t.Fatalf("last progress %+v", last)
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dl, "proj", filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %v (%d bytes)", rel, err, len(got))
		}
	}
	if st, err := os.Stat(filepath.Join(dl, "proj", "empty", "nested")); err != nil || !st.IsDir() {
		t.Fatalf("empty dir: %v", err)
	}

	// The same folder again lands beside the first, never inside it.
	res, err = ReceiveBody(NewExtractor(dl, p.Manifest, nil), p.Manifest, bytes.NewReader(plain))
	if err != nil || res.Paths[0] != filepath.Join(dl, "proj (1)") {
		t.Fatalf("second receive: %+v %v", res, err)
	}
	assertNoPartsTree(t, dl)
}

// TestBodyFromResumePoint: a body can begin in the middle of an entry; the
// earlier bytes are skipped (by seeking when the source can).
func TestBodyFromResumePoint(t *testing.T) {
	src := t.TempDir()
	mkTree(t, filepath.Join(src, "d"), map[string]string{"a.txt": "alpha", "b.txt": "0123456789", "c.txt": "gamma"})
	p, err := PlanPaths([]string{filepath.Join(src, "d")})
	if err != nil {
		t.Fatal(err)
	}
	// Entries: d/, d/a.txt, d/b.txt, d/c.txt.
	s := Start{Entry: 2, Offset: 4}
	plain := bodyOf(t, p, s)
	h := s.Header()
	if !bytes.Equal(plain[:StartHeaderSize], h[:]) || string(plain[StartHeaderSize:]) != "456789gamma" {
		t.Fatalf("body %q", plain)
	}
	if BodySize(p.Manifest, s) != int64(len(plain)) {
		t.Fatalf("BodySize %d, body %d", BodySize(p.Manifest, s), len(plain))
	}
	if got, err := ReadStart(bytes.NewReader(plain)); err != nil || got != s {
		t.Fatalf("ReadStart %+v %v", got, err)
	}
	// A source that cannot seek is read and dropped up to the offset.
	var buf bytes.Buffer
	err = writeBody(&buf, p.Manifest, s, func(i int, _ protocol.Entry) (io.ReadCloser, error) {
		b, err := os.ReadFile(p.Sources[i])
		return io.NopCloser(iotest.HalfReader(bytes.NewReader(b))), err
	}, newTracker(nil, 0, 0))
	if err != nil || !bytes.Equal(buf.Bytes(), plain) {
		t.Fatalf("non-seeking source: %v %q", err, buf.Bytes())
	}
	// Points outside the manifest are refused.
	for _, bad := range []Start{{Entry: 0, Offset: 1}, {Entry: 2, Offset: 11}, {Entry: 9}} {
		if err := writeBody(io.Discard, p.Manifest, bad, nil, newTracker(nil, 0, 0)); !errors.Is(err, ErrStart) {
			t.Errorf("start %+v: %v", bad, err)
		}
	}
}

func TestCheckStart(t *testing.T) {
	offered := &protocol.ResumePoint{Entry: 3, Offset: 1 << 20, SHA256: strings.Repeat("a", 64)}
	type tc struct {
		s Start
		p *protocol.ResumePoint
	}
	for _, c := range []tc{{Start{}, nil}, {Start{}, offered}, {Start{Entry: 3, Offset: 1 << 20}, offered}} {
		if err := CheckStart(c.s, c.p); err != nil {
			t.Errorf("%+v with %+v: %v", c.s, c.p, err)
		}
	}
	for _, c := range []tc{
		{Start{Entry: 3, Offset: 1 << 20}, nil}, {Start{Entry: 3, Offset: 1}, offered},
		{Start{Entry: 2, Offset: 1 << 20}, offered}, {Start{Offset: 5}, nil},
	} {
		if err := CheckStart(c.s, c.p); !errors.Is(err, ErrStart) {
			t.Errorf("%+v with %+v: %v", c.s, c.p, err)
		}
	}
}

// TestReceiveBodyRejectsHostileBodies feeds plaintext bodies that do not
// match an accepted manifest {d/, d/a.txt (5 bytes), d/b.txt (3 bytes)}:
// each must fail and leave the download folder (and its parent) untouched,
// including a file that was complete before the failure.
func TestReceiveBodyRejectsHostileBodies(t *testing.T) {
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "d", Mode: protocol.ModeDir | 0o755}, {Path: "d/a.txt", Size: 5, Mode: 0o644}, {Path: "d/b.txt", Size: 3, Mode: 0o644},
	}, Total: 8}
	hdr := func(s Start) string { h := s.Header(); return string(h[:]) }
	zero := hdr(Start{})
	padded := []byte(zero)
	padded[15] = 1
	cases := map[string]struct {
		body string
		want error
	}{
		"empty":             {"", ErrSize},
		"short header":      {zero[:9], ErrSize},
		"padding not zero":  {string(padded) + "hellobee", ErrStart},
		"start not offered": {hdr(Start{Entry: 2, Offset: 1}) + "ee", ErrStart},
		"short":             {zero + "hellobe", ErrSize},
		"only first file":   {zero + "hello", ErrSize},
		"extra data":        {zero + "hellobee!", ErrSize},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dl := filepath.Join(parent, "downloads")
			if err := os.Mkdir(dl, 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := ReceiveBody(NewExtractor(dl, m, nil), m, strings.NewReader(c.body))
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if left, _ := os.ReadDir(dl); len(left) != 0 {
				t.Fatalf("left in downloads: %v", left)
			}
			if left, _ := os.ReadDir(parent); len(left) != 1 {
				t.Fatalf("written beside downloads: %v", left)
			}
		})
	}
	// A stream error after the last byte (a bad final frame) still removes
	// everything, and keeps its identity.
	dl := t.TempDir()
	src := io.MultiReader(strings.NewReader(zero+"hellobee"), iotest.ErrReader(securestream.ErrAuth))
	if _, err := ReceiveBody(NewExtractor(dl, m, nil), m, src); !errors.Is(err, securestream.ErrAuth) {
		t.Fatalf("bad end: %v", err)
	}
	if left, _ := os.ReadDir(dl); len(left) != 0 {
		t.Fatalf("left after a bad end: %v", left)
	}
	// A stream cut off in the middle of a file is not reported as a short
	// body: it keeps its identity.
	src = io.MultiReader(strings.NewReader(zero+"hel"), iotest.ErrReader(securestream.ErrTruncated))
	if _, err := ReceiveBody(NewExtractor(dl, m, nil), m, src); !errors.Is(err, securestream.ErrTruncated) || errors.Is(err, ErrSize) {
		t.Fatalf("cut off: %v", err)
	}
	if left, _ := os.ReadDir(dl); len(left) != 0 {
		t.Fatalf("left after a cut: %v", left)
	}
}
