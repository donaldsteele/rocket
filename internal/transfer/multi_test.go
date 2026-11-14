package transfer

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/donaldsteele/rocket/internal/protocol"
)

// mkTree writes files (relative "/" paths -> content) and empty dirs under root.
func mkTree(t *testing.T, root string, files map[string]string, dirs ...string) {
	t.Helper()
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func paths(m protocol.Manifest) []string {
	var out []string
	for _, e := range m.Entries {
		p := e.Path
		if e.Mode&protocol.ModeDir != 0 {
			p += "/"
		}
		out = append(out, p)
	}
	return out
}

func TestPlanPathsFolder(t *testing.T) {
	src := t.TempDir()
	mkTree(t, filepath.Join(src, "proj"), map[string]string{
		"a.txt":         "alpha",
		"sub/deep/b.md": "beta",
		"zero.bin":      "",
		"ünï 文件.txt":    "unicode",
	}, "empty")
	mkTree(t, filepath.Join(src, "other", "proj"), map[string]string{"c.txt": "gamma"})
	loose := filepath.Join(src, "loose.txt")
	mkTree(t, src, map[string]string{"loose.txt": "x"})

	p, err := PlanPaths([]string{filepath.Join(src, "proj"), loose, filepath.Join(src, "other", "proj")})
	if err != nil {
		t.Fatal(err)
	}
	m := p.Manifest
	want := []string{
		"proj/", "proj/a.txt", "proj/empty/", "proj/sub/", "proj/sub/deep/", "proj/sub/deep/b.md",
		"proj/zero.bin", "proj/ünï 文件.txt", "loose.txt", "proj (1)/", "proj (1)/c.txt",
	}
	if got := paths(m); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries:\n got %q\nwant %q", got, want)
	}
	if m.Kind != protocol.KindMulti || m.Total != int64(len("alpha")+len("beta")+len("unicode")+1+len("gamma")) {
		t.Fatalf("kind %s total %d", m.Kind, m.Total)
	}
	if Files(m) != 6 || Label(m) != "proj" || len(p.Sources) != len(m.Entries) {
		t.Fatalf("files %d label %q sources %d", Files(m), Label(m), len(p.Sources))
	}
	for i, e := range m.Entries {
		if (e.Mode&protocol.ModeDir != 0) != (p.Sources[i] == "") {
			t.Fatalf("entry %s source %q", e.Path, p.Sources[i])
		}
	}

	// One regular file is a plain kind:file offer.
	p, err = PlanPaths([]string{loose})
	if err != nil || p.Manifest.Kind != protocol.KindFile || p.Manifest.Entries[0].Path != "loose.txt" {
		t.Fatalf("single file plan: %+v %v", p, err)
	}
	if _, err := PlanPaths([]string{filepath.Join(src, "missing")}); err == nil {
		t.Fatal("missing path planned")
	}
}

func TestPlanPathsSkipsSymlinks(t *testing.T) {
	src := t.TempDir()
	mkTree(t, filepath.Join(src, "d"), map[string]string{"real.txt": "r"})
	if err := os.Symlink(filepath.Join(src, "d", "real.txt"), filepath.Join(src, "d", "link.txt")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	p, err := PlanPaths([]string{filepath.Join(src, "d")})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(p.Manifest), "|"); got != "d/|d/real.txt" {
		t.Fatalf("entries %s", got)
	}
	if len(p.Skipped) != 1 || filepath.Base(p.Skipped[0]) != "link.txt" {
		t.Fatalf("skipped %v", p.Skipped)
	}
}

func TestBrowserManifest(t *testing.T) {
	m, err := BrowserManifest([]BrowserFile{
		{Name: "photos/2024/a.jpg", Size: 3}, {Name: "photos/2024/b.jpg", Size: 4},
		{Name: "photos/../x:y.txt", Size: 1}, {Name: "A.JPG", Size: 0}, {Name: "a.jpg", Size: 2},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := "photos/|photos/2024/|photos/2024/a.jpg|photos/2024/b.jpg|photos/x_y.txt|A.JPG|a (1).jpg"
	if got := strings.Join(paths(m), "|"); got != want || m.Total != 10 || Files(m) != 5 {
		t.Fatalf("got %s total %d", got, m.Total)
	}
	// Without folders every name is a bare base name.
	m, err = BrowserManifest([]BrowserFile{{Name: `C:\fakepath\b.txt`, Size: 1}, {Name: "d/b.txt", Size: 1}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(m), "|"); got != "b.txt|b (1).txt" {
		t.Fatalf("bare names %s", got)
	}
	m, _ = BrowserManifest([]BrowserFile{{Name: "one.txt", Size: 1}}, true)
	if m.Kind != protocol.KindFile {
		t.Fatalf("one bare file is kind %s", m.Kind)
	}
}

// tarOf streams a plan through writeTar into memory.
func tarOf(t *testing.T, p *Plan) []byte {
	t.Helper()
	var buf bytes.Buffer
	err := writeTar(&buf, p.Manifest, func(i int, _ protocol.Entry) (io.ReadCloser, error) {
		return os.Open(p.Sources[i])
	}, newTracker(nil, p.Manifest.Total, Files(p.Manifest)))
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTarRoundTrip(t *testing.T) {
	src := t.TempDir()
	files := map[string]string{"a.txt": "alpha", "sub/b.bin": strings.Repeat("z", 70000), "zero": "", "ü/文件.txt": "unicode"}
	mkTree(t, filepath.Join(src, "proj"), files, "empty/nested")
	p, err := PlanPaths([]string{filepath.Join(src, "proj")})
	if err != nil {
		t.Fatal(err)
	}
	body := tarOf(t, p)

	dl := t.TempDir()
	var last Progress
	res, err := ReceiveTar(dl, p.Manifest, bytes.NewReader(body), func(pr Progress) { last = pr })
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
	res, err = ReceiveTar(dl, p.Manifest, bytes.NewReader(body), nil)
	if err != nil || res.Paths[0] != filepath.Join(dl, "proj (1)") {
		t.Fatalf("second receive: %+v %v", res, err)
	}
	assertNoPartsTree(t, dl)
}

type tarEntry struct {
	name string
	typ  byte
	data string
	link string
}

func rawTar(entries ...tarEntry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		_ = tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: e.typ, Size: int64(len(e.data)), Mode: 0o644, Linkname: e.link})
		if e.typ == tar.TypeReg {
			_, _ = io.WriteString(tw, e.data)
		}
	}
	_ = tw.Close()
	return buf.Bytes()
}

// TestReceiveTarRejectsHostileArchives feeds archives that do not match an
// accepted manifest {d/, d/a.txt (5 bytes)}: each must fail with ErrArchive or
// ErrSize and leave the download folder (and its parent) untouched.
func TestReceiveTarRejectsHostileArchives(t *testing.T) {
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "d", Mode: protocol.ModeDir | 0o755}, {Path: "d/a.txt", Size: 5, Mode: 0o644},
	}, Total: 5}
	dir := byte(tar.TypeDir)
	reg := byte(tar.TypeReg)
	cases := map[string][]byte{
		"traversal name":   rawTar(tarEntry{name: "d/", typ: dir}, tarEntry{name: "d/../../a.txt", typ: reg, data: "hello"}),
		"absolute name":    rawTar(tarEntry{name: "/d/", typ: dir}, tarEntry{name: "/d/a.txt", typ: reg, data: "hello"}),
		"backslash name":   rawTar(tarEntry{name: "d/", typ: dir}, tarEntry{name: `d\..\..\a.txt`, typ: reg, data: "hello"}),
		"symlink":          rawTar(tarEntry{name: "d/", typ: dir}, tarEntry{name: "d/a.txt", typ: tar.TypeSymlink, link: "/etc/passwd"}),
		"hard link":        rawTar(tarEntry{name: "d/", typ: dir}, tarEntry{name: "d/a.txt", typ: tar.TypeLink, link: "../../x"}),
		"device":           rawTar(tarEntry{name: "d/", typ: dir}, tarEntry{name: "d/a.txt", typ: tar.TypeChar}),
		"dir for file":     rawTar(tarEntry{name: "d/", typ: dir}, tarEntry{name: "d/a.txt/", typ: dir}),
		"size mismatch":    rawTar(tarEntry{name: "d/", typ: dir}, tarEntry{name: "d/a.txt", typ: reg, data: "hello world"}),
		"missing entry":    rawTar(tarEntry{name: "d/", typ: dir}),
		"extra entry":      rawTar(tarEntry{name: "d/", typ: dir}, tarEntry{name: "d/a.txt", typ: reg, data: "hello"}, tarEntry{name: "d/b.txt", typ: reg, data: "x"}),
		"wrong order":      rawTar(tarEntry{name: "d/a.txt", typ: reg, data: "hello"}, tarEntry{name: "d/", typ: dir}),
		"truncated":        rawTar(tarEntry{name: "d/", typ: dir}, tarEntry{name: "d/a.txt", typ: reg, data: "hello"})[:1024+2],
		"not a tar at all": []byte(strings.Repeat("garbage!", 200)),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dl := filepath.Join(parent, "downloads")
			if err := os.Mkdir(dl, 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := ReceiveTar(dl, m, bytes.NewReader(body), nil)
			if !errors.Is(err, ErrArchive) && !errors.Is(err, ErrSize) {
				t.Fatalf("err = %v", err)
			}
			if left, _ := os.ReadDir(dl); len(left) != 0 {
				t.Fatalf("left in downloads: %v", left)
			}
			if left, _ := os.ReadDir(parent); len(left) != 1 {
				t.Fatalf("written beside downloads: %v", left)
			}
		})
	}
}

func TestExtractorRejectsBadManifestPaths(t *testing.T) {
	dl := t.TempDir()
	for _, p := range []string{"../evil", "a/../../evil", "/abs", `a\b`, "C:/x"} {
		m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{{Path: p, Size: 1}}, Total: 1}
		x := NewExtractor(dl, m, nil)
		if err := x.Write(0, strings.NewReader("x")); !errors.Is(err, ErrArchive) {
			t.Errorf("Write(%q) = %v", p, err)
		}
		x.Abort()
	}
	if left, _ := os.ReadDir(dl); len(left) != 0 {
		t.Fatalf("left: %v", left)
	}
}

func TestExtractorAbortRemovesEarlierFiles(t *testing.T) {
	dl := t.TempDir()
	mkTree(t, dl, map[string]string{"keep.txt": "mine"})
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "keep.txt", Size: 3}, {Path: "f/x", Size: 1}, {Path: "g/y", Size: 4},
	}, Total: 8}
	x := NewExtractor(dl, m, nil)
	if err := x.Write(0, strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	if err := x.Write(1, strings.NewReader("1")); err != nil {
		t.Fatal(err)
	}
	if err := x.Write(2, strings.NewReader("ab")); !errors.Is(err, ErrSize) {
		t.Fatalf("short entry: %v", err)
	}
	x.Abort()
	left, _ := os.ReadDir(dl)
	if len(left) != 1 || left[0].Name() != "keep.txt" {
		t.Fatalf("left after abort: %v", left)
	}
	if got, _ := os.ReadFile(filepath.Join(dl, "keep.txt")); string(got) != "mine" {
		t.Fatalf("existing file changed: %q", got)
	}
}

func TestSanitizedNameClash(t *testing.T) {
	// "a?" and "a*" both sanitize to "a_": the second must fail, not overwrite.
	dl := t.TempDir()
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: []protocol.Entry{
		{Path: "d/a?", Size: 1}, {Path: "d/a*", Size: 1},
	}, Total: 2}
	x := NewExtractor(dl, m, nil)
	if err := x.Write(0, strings.NewReader("1")); err != nil {
		t.Fatal(err)
	}
	if err := x.Write(1, strings.NewReader("2")); !errors.Is(err, ErrArchive) {
		t.Fatalf("clash: %v", err)
	}
	x.Abort()
}

func assertNoPartsTree(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, PartSuffix) {
			t.Errorf("leftover part file %s", p)
		}
		return nil
	})
}
