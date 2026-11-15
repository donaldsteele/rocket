package receiver

import (
	"bytes"
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

// TestFilesSinkThroughExtractTar: the tar checks run before the sink sees
// anything of a bad entry.
func TestFilesSinkThroughExtractTar(t *testing.T) {
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "d", Mode: protocol.ModeDir | 0o755},
		{Path: "d/a", Size: 3, Mode: 0o644},
	}, Total: 3}
	files := newMemFiles()
	_, err := transfer.ExtractTar(begin(t, files, m), m, io.LimitReader(strings.NewReader("not a tar archive at all"), 1<<20))
	if !errors.Is(err, transfer.ErrArchive) || !files.aborted || len(files.ops) != 0 {
		t.Fatalf("err %v aborted %v ops %q", err, files.aborted, files.ops)
	}
}
