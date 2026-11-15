package transfer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeComponent(t *testing.T) {
	cases := map[string]string{
		"report.pdf":                 "report.pdf",
		"a:b":                        "a_b",
		`a\b/c`:                      "a_b_c",
		"name. . ":                   "name",
		"...":                        "_",
		"":                           "_",
		"CON":                        "_CON",
		"con.txt":                    "_con.txt",
		"Lpt1.tar.gz":                "_Lpt1.tar.gz",
		"COM10":                      "COM10",
		"console":                    "console",
		"a\x00b\nc":                  "a_b_c",
		`what?<>|*".txt`:             "what______.txt",
		"ファイル.txt":                   "ファイル.txt",
		"invoice\u202efdp.exe":       "invoice_fdp.exe",
		"a\u2066b\u2069c":            "a_b_c",
		"zero\u200bwidth":            "zero_width",
		"bom\ufeff.txt":              "bom_.txt",
		"c1\u0085x":                  "c1_x",
		"\U0001F468\u200d\U0001F469": "\U0001F468\u200d\U0001F469",
		"COM\u00b9.txt":              "_COM\u00b9.txt",
		"lpt\u00b3":                  "_lpt\u00b3",
		"COM\u00b2":                  "_COM\u00b2",
		"LPT\u00b9.log":              "_LPT\u00b9.log",
		"COM\u00b9x":                 "COM\u00b9x",
	}
	for in, want := range cases {
		if got := SanitizeComponent(in); got != want {
			t.Errorf("SanitizeComponent(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 200) // 400 bytes
	if got := SanitizeComponent(long); len(got) > MaxNameBytes || !strings.HasPrefix(long, got) {
		t.Errorf("long name not cut on a rune boundary: %d bytes", len(got))
	}
	longExt := strings.Repeat("a", 250) + ".pdf"
	if got := SanitizeComponent(longExt); len(got) > MaxNameBytes || !strings.HasSuffix(got, ".pdf") || !strings.HasPrefix(got, "aaa") {
		t.Errorf("long name with extension = %q (%d bytes)", got, len(got))
	}
	if got := SanitizeComponent(strings.Repeat("a", 240) + ". . .pdf"); strings.HasSuffix(got, " ") || strings.HasSuffix(got, ".") {
		t.Errorf("shortened name ends in a dot or space: %q", got)
	}
	if got := SanitizeComponent("CON" + strings.Repeat(".x", 200)); len(got) > MaxNameBytes || got[0] != '_' {
		t.Errorf("long reserved name = %q (%d bytes)", got, len(got))
	}
	if got := SanitizePath("a\u202e/b:c"); got != "a_/b_c" {
		t.Errorf("SanitizePath = %q", got)
	}
	if got := BaseName(`C:\Users\x\Desktop\photo.jpg`); got != "photo.jpg" {
		t.Errorf("BaseName = %q", got)
	}
	if got := BaseName("../../etc/passwd"); got != "passwd" {
		t.Errorf("BaseName = %q", got)
	}
}

func TestSafeJoinTraversal(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"../x", "a/../../x", "..", ".", "a//b", ""} {
		if p, err := SafeJoin(root, rel); err == nil {
			t.Errorf("SafeJoin(%q) = %q, want error", rel, p)
		}
	}
	p, err := SafeJoin(root, "dir/CON/a:b.")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "dir", "_CON", "a_b")
	if p != want {
		t.Fatalf("SafeJoin = %q, want %q", p, want)
	}
	// A segment that only looks like a separator on Windows stays inside.
	p, err = SafeJoin(root, `..\..\evil`)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != filepath.Clean(root) {
		t.Fatalf("backslash segment escaped: %q", p)
	}
}

func TestReceiveFileExact(t *testing.T) {
	dir := t.TempDir()
	data := []byte("hello rocket")
	var last int64
	p, err := ReceiveFile(dir, "greeting.txt", int64(len(data)), bytes.NewReader(data), func(p Progress) { last = p.Sent })
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) != "greeting.txt" {
		t.Fatalf("final name %q", p)
	}
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, data) || last != int64(len(data)) {
		t.Fatalf("content %q progress %d", got, last)
	}
	// Second file with the same name gets a unique name, never overwrites.
	p2, err := ReceiveFile(dir, "greeting.txt", 1, bytes.NewReader([]byte("x")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p2) != "greeting (1).txt" {
		t.Fatalf("second name %q", p2)
	}
	assertNoParts(t, dir)
}

func TestReceiveFileSizeMismatchRemovesPartial(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReceiveFile(dir, "short.bin", 10, bytes.NewReader([]byte("abc")), nil); !errors.Is(err, ErrSize) {
		t.Fatalf("short body: %v", err)
	}
	if _, err := ReceiveFile(dir, "long.bin", 2, bytes.NewReader([]byte("abc")), nil); !errors.Is(err, ErrSize) {
		t.Fatalf("long body: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("partials left behind: %v", entries)
	}
}

func TestReceiveFileTraversalName(t *testing.T) {
	dir := t.TempDir()
	p, err := ReceiveFile(dir, "../../escape.txt", 1, bytes.NewReader([]byte("x")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != dir {
		t.Fatalf("file written outside the download dir: %s", p)
	}
}

func assertNoParts(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), PartSuffix) {
			t.Fatalf("leftover part file %s", e.Name())
		}
	}
}
