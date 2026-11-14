package main

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteRootEntriesAndExecBit(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "pkg")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"rocket": "binary", "README.md": "# rocket\n"}
	var paths []string
	for name, body := range files {
		p := filepath.Join(src, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	out := filepath.Join(dir, "out.zip")
	if err := write(out, paths, []string{"rocket"}); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if len(zr.File) != len(files) {
		t.Fatalf("got %d entries, want %d", len(zr.File), len(files))
	}
	for _, f := range zr.File {
		want, ok := files[f.Name]
		if !ok {
			t.Fatalf("unexpected entry %q (entries must sit at the zip root)", f.Name)
		}
		wantMode := os.FileMode(0o644)
		if f.Name == "rocket" {
			wantMode = 0o755
		}
		if got := f.Mode().Perm(); got != wantMode {
			t.Errorf("%s: mode %v, want %v", f.Name, got, wantMode)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want {
			t.Errorf("%s: content %q, want %q", f.Name, b, want)
		}
	}
}
