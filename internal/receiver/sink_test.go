package receiver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// memFiles is an in-memory Files that records what it was asked to do.
type memFiles struct {
	taken   map[string]bool // top-level names that exist already
	ops     []string
	files   map[string]*bytes.Buffer
	aborted bool
}

func newMemFiles(taken ...string) *memFiles {
	m := &memFiles{taken: map[string]bool{}, files: map[string]*bytes.Buffer{}}
	for _, n := range taken {
		m.taken[n] = true
	}
	return m
}

func (m *memFiles) Mkdir(rel string) (string, error) {
	if !strings.Contains(rel, "/") {
		base := rel
		for n := 1; m.taken[rel]; n++ {
			rel = fmt.Sprintf("%s (%d)", base, n)
		}
		m.taken[rel] = true
	}
	m.ops = append(m.ops, "mkdir "+rel)
	return rel, nil
}

func (m *memFiles) Create(rel string, size int64, mode uint32) (File, error) {
	m.ops = append(m.ops, fmt.Sprintf("create %s %d %o", rel, size, mode))
	b := &bytes.Buffer{}
	m.files[rel] = b
	return &memFile{m: m, rel: rel, b: b}, nil
}

func (m *memFiles) Abort()           { m.aborted = true }
func (m *memFiles) Location() string { return "mem:" }

type memFile struct {
	m   *memFiles
	rel string
	b   *bytes.Buffer
}

func (f *memFile) Write(p []byte) (int, error) { return f.b.Write(p) }
func (f *memFile) Commit() (string, error) {
	f.m.ops = append(f.m.ops, "commit "+f.rel)
	return "mem:" + f.rel, nil
}
func (f *memFile) Abort() { f.m.ops = append(f.m.ops, "abort "+f.rel) }

func begin(t *testing.T, files *memFiles, m protocol.Manifest) Output {
	t.Helper()
	out, err := FilesSink(func(string, protocol.Manifest) (Files, error) { return files, nil }).Begin("t1", m, nil)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFilesSinkWritesTree(t *testing.T) {
	rlo := string(rune(0x202e))
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "photos", Mode: protocol.ModeDir | 0o755},
		{Path: "photos/a" + rlo + "gpj.exe", Size: 3, Mode: 0o755},
		{Path: "photos/sub", Mode: protocol.ModeDir | 0o755},
		{Path: "Docs/x.txt", Size: 1, Mode: 0o644}, // no folder entry first
		{Path: "docs/y.txt", Size: 0, Mode: 0o644}, // same folder, other case
		{Path: "top.txt", Size: 2, Mode: 0o644},
	}, Total: 6}
	files := newMemFiles("photos")
	out := begin(t, files, m)
	bodies := []string{"", "abc", "", "x", "", "hi"}
	for i, b := range bodies {
		if err := out.Write(i, strings.NewReader(b)); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
	res, err := out.Finish()
	if err != nil {
		t.Fatal(err)
	}
	wantOps := []string{
		"mkdir photos (1)",
		"create photos (1)/a_gpj.exe 3 755", "commit photos (1)/a_gpj.exe",
		"mkdir photos (1)/sub",
		"mkdir Docs",
		"create Docs/x.txt 1 644", "commit Docs/x.txt",
		"create Docs/y.txt 0 644", "commit Docs/y.txt",
		"create top.txt 2 644", "commit top.txt",
	}
	if !slices.Equal(files.ops, wantOps) {
		t.Fatalf("ops\n%q\nwant\n%q", files.ops, wantOps)
	}
	if !slices.Equal(res.Paths, []string{"photos (1)", "Docs", "mem:top.txt"}) || res.Files != 4 || res.Bytes != 6 {
		t.Fatalf("result %+v", res)
	}
	if files.files["top.txt"].String() != "hi" || out.Location() != "mem:" {
		t.Fatal("content or location wrong")
	}
}

func TestFilesSinkSizeMismatch(t *testing.T) {
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "d", Mode: protocol.ModeDir | 0o755},
		{Path: "d/a", Size: 3, Mode: 0o644},
	}, Total: 3}
	for _, body := range []string{"ab", "abcd"} {
		files := newMemFiles()
		out := begin(t, files, m)
		if err := out.Write(0, nil); err != nil {
			t.Fatal(err)
		}
		err := out.Write(1, strings.NewReader(body))
		if !errors.Is(err, transfer.ErrSize) {
			t.Fatalf("body %q: %v, want ErrSize", body, err)
		}
		if files.ops[len(files.ops)-1] != "abort d/a" {
			t.Fatalf("body %q: ops %q", body, files.ops)
		}
		out.Abort()
		if !files.aborted {
			t.Fatal("transfer not aborted")
		}
	}
	// Out of order, and Finish before the end.
	out := begin(t, newMemFiles(), m)
	if err := out.Write(1, strings.NewReader("abc")); !errors.Is(err, transfer.ErrArchive) {
		t.Fatalf("out of order: %v", err)
	}
	if _, err := out.Finish(); !errors.Is(err, transfer.ErrArchive) {
		t.Fatalf("early finish: %v", err)
	}
}

// TestFilesSinkThroughReceiveBody: a bad start header fails before the sink
// sees anything, and a body that goes wrong after a file was committed
// removes that file too.
func TestFilesSinkThroughReceiveBody(t *testing.T) {
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "d", Mode: protocol.ModeDir | 0o755},
		{Path: "d/a", Size: 3, Mode: 0o644},
	}, Total: 3}
	files := newMemFiles()
	_, err := transfer.ReceiveBody(begin(t, files, m), m, strings.NewReader("not a start header at all"))
	if !errors.Is(err, transfer.ErrStart) || !files.aborted || len(files.ops) != 0 {
		t.Fatalf("err %v aborted %v ops %q", err, files.aborted, files.ops)
	}
	h := transfer.Start{}.Header()
	files = newMemFiles()
	_, err = transfer.ReceiveBody(begin(t, files, m), m, io.MultiReader(bytes.NewReader(h[:]), strings.NewReader("abc+")))
	if !errors.Is(err, transfer.ErrSize) || !files.aborted || !slices.Contains(files.ops, "commit d/a") {
		t.Fatalf("err %v aborted %v ops %q", err, files.aborted, files.ops)
	}
}

// keepFiles is a memFiles that can keep (KeepFiles), shared by every
// attempt of a transfer as a phone's storage would be.
type keepFiles struct {
	*memFiles
	kept    bool
	reopens []string
}

func (k *keepFiles) Create(rel string, size int64, mode uint32) (File, error) {
	f, err := k.memFiles.Create(rel, size, mode)
	if err != nil {
		return nil, err
	}
	return &keptMemFile{memFile: f.(*memFile)}, nil
}

func (k *keepFiles) Keep() (json.RawMessage, error) {
	k.kept = true
	return json.RawMessage(`{"store":"mem"}`), nil
}

func (k *keepFiles) Reopen(ref string, offset int64) (KeptFile, error) {
	b, ok := k.files[ref]
	if !ok || int64(b.Len()) < offset {
		return nil, fmt.Errorf("no incomplete %s", ref)
	}
	b.Truncate(int(offset))
	k.reopens = append(k.reopens, fmt.Sprintf("%s@%d", ref, offset))
	return &keptMemFile{memFile: &memFile{m: k.memFiles, rel: ref, b: b}}, nil
}

type keptMemFile struct{ *memFile }

func (f *keptMemFile) Ref() string  { return f.rel }
func (f *keptMemFile) Close() error { return nil }

// keepStore is a FilesStore over one keepFiles.
type keepStore struct {
	files     *keepFiles
	states    []string
	discarded int
}

func (s *keepStore) Begin(string, protocol.Manifest) (KeepFiles, error) { return s.files, nil }
func (s *keepStore) Resume(_ string, _ protocol.Manifest, st json.RawMessage) (KeepFiles, error) {
	s.states = append(s.states, string(st))
	return s.files, nil
}
func (s *keepStore) Check(json.RawMessage) error { return nil }
func (s *keepStore) Discard(json.RawMessage)     { s.discarded++ }

// cutReader gives n bytes of data, then fails as a lost connection does.
type cutReader struct {
	r *strings.Reader
	n int
}

func (c *cutReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, fmt.Errorf("read body: %w", transfer.ErrConnLost)
	}
	p = p[:min(len(p), c.n)]
	k, err := c.r.Read(p)
	c.n -= k
	return k, err
}

// TestFilesSinkKeepAndResume: a network failure mid-file keeps what was
// written with the SHA-256 of the written part as the resume point, and a
// resumed output continues the same file from there.
func TestFilesSinkKeepAndResume(t *testing.T) {
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "d", Mode: protocol.ModeDir | 0o755},
		{Path: "d/a", Size: 3, Mode: 0o644},
		{Path: "d/b", Size: 10, Mode: 0o644},
		{Path: "d/e", Mode: protocol.ModeDir | 0o755},
	}, Total: 13}
	store := &keepStore{files: &keepFiles{memFiles: newMemFiles()}}
	sink := NewFilesSink(store)
	out, err := sink.Begin("t1", m, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range []string{"", "abc"} {
		if err := out.Write(i, strings.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	err = out.Write(2, &cutReader{r: strings.NewReader("0123456789"), n: 4})
	if !transfer.IsNetwork(err) {
		t.Fatalf("cut write: %v", err)
	}
	ko, err := out.(Keeper).Keep()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("0123"))
	want := protocol.ResumePoint{Entry: 2, Offset: 4, SHA256: hex.EncodeToString(sum[:])}
	if ko.Point != want || ko.Next != 2 || ko.Done != 7 || !store.files.kept {
		t.Fatalf("kept %+v, want point %+v", ko, want)
	}
	if slices.Contains(store.files.ops, "abort d/b") || store.files.aborted {
		t.Fatalf("keep removed something: %q", store.files.ops)
	}
	if err := sink.Check(ko.State); err != nil {
		t.Fatal(err)
	}

	// The next attempt resumes at the point; the part file is reopened.
	var last transfer.Progress
	out, err = sink.Resume("t2", m, ko.State, func(p transfer.Progress) { last = p })
	if err != nil {
		t.Fatal(err)
	}
	if len(store.states) != 1 || store.states[0] != `{"store":"mem"}` {
		t.Fatalf("sink state %q", store.states)
	}
	res, err := transfer.ReceiveFrom(out, m, ko.Next, transfer.Start{Entry: 2, Offset: 4}, strings.NewReader("456789"))
	if err != nil {
		t.Fatal(err)
	}
	if got := store.files.files["d/b"].String(); got != "0123456789" {
		t.Fatalf("d/b = %q", got)
	}
	if !slices.Equal(store.files.reopens, []string{"d/b@4"}) || res.Files != 2 || res.Bytes != 13 || !slices.Equal(res.Paths, []string{"d"}) {
		t.Fatalf("reopens %q result %+v", store.files.reopens, res)
	}
	if last.Sent != 13 || last.FileIndex != 2 {
		t.Fatalf("last progress %+v", last)
	}

	// Broken again before reaching the part: the same point is kept.
	out, err = sink.Resume("t3", m, ko.State, nil)
	if err != nil {
		t.Fatal(err)
	}
	ko2, err := out.(Keeper).Keep()
	if err != nil || ko2.Point != want || ko2.Done != 7 {
		t.Fatalf("kept again %+v %v", ko2, err)
	}
	sink.Discard(ko2.State)
	if store.discarded != 1 {
		t.Fatal("discard did not reach the store")
	}
}

// TestFilesSinkKeepPoints: with no file in flight the point is the next
// file at 0, or, when every file is stored, the last file at its end; a
// plain FilesSink cannot keep, and a non-network failure is not kept.
func TestFilesSinkKeepPoints(t *testing.T) {
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "a", Size: 2, Mode: 0o644},
		{Path: "b", Size: 1, Mode: 0o644},
	}, Total: 3}
	empty := sha256.Sum256(nil)
	sink := NewFilesSink(&keepStore{files: &keepFiles{memFiles: newMemFiles()}})
	out, _ := sink.Begin("t1", m, nil)
	if err := out.Write(0, strings.NewReader("hi")); err != nil {
		t.Fatal(err)
	}
	ko, err := out.(Keeper).Keep()
	if err != nil || ko.Point != (protocol.ResumePoint{Entry: 1, SHA256: hex.EncodeToString(empty[:])}) {
		t.Fatalf("next-file point %+v %v", ko.Point, err)
	}
	out, _ = sink.Resume("t2", m, ko.State, nil)
	if err := out.Write(1, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	ko, err = out.(Keeper).Keep()
	sum := sha256.Sum256([]byte("x"))
	if err != nil || ko.Point != (protocol.ResumePoint{Entry: 1, Offset: 1, SHA256: hex.EncodeToString(sum[:])}) || ko.Done != 3 {
		t.Fatalf("end point %+v %v", ko, err)
	}

	files := newMemFiles()
	plain := begin(t, files, m)
	if _, err := plain.(Keeper).Keep(); err == nil {
		t.Fatal("a plain FilesSink kept a transfer")
	}

	store := &keepStore{files: &keepFiles{memFiles: newMemFiles()}}
	out, _ = NewFilesSink(store).Begin("t3", m, nil)
	if err := out.Write(0, strings.NewReader("hey")); !errors.Is(err, transfer.ErrSize) {
		t.Fatalf("oversized: %v", err)
	}
	if !slices.Contains(store.files.ops, "abort a") {
		t.Fatalf("an oversized file was left: %q", store.files.ops)
	}
	if _, err := NewFilesSink(store).Resume("t4", m, json.RawMessage(`{"next":5}`), nil); !errors.Is(err, transfer.ErrArchive) {
		t.Fatalf("resume of a state that does not fit: %v", err)
	}
}
