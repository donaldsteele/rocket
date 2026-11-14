package history

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func entry(i int) Entry {
	t := time.Unix(1_700_000_000+int64(i), 0)
	return Entry{ID: fmt.Sprintf("t%04d", i), Dir: DirOut, Name: "f.txt", PeerName: "bob", Files: 1, Bytes: int64(i), Started: t, Ended: t, Result: ResultOK}
}

func lines(t *testing.T, s *Store) int {
	t.Helper()
	b, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func TestAddListNewestFirst(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if err := s.Add(entry(i)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "t0004" || got[2].ID != "t0002" {
		t.Fatalf("List(3) = %+v", got)
	}
	all, _ := s.List(0)
	if len(all) != 5 {
		t.Fatalf("List(0) returned %d entries", len(all))
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.List(0); len(all) != 0 {
		t.Fatalf("after Clear: %d entries", len(all))
	}
}

// TestCapAndCompaction: opening a file with more than Cap entries (and a torn
// line) keeps the newest Cap valid ones; a long-running store compacts again
// once it has grown past the cap.
func TestCapAndCompaction(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := range Cap + 250 {
		if i == 10 {
			b.WriteString("{not json\n")
		}
		s := fmt.Sprintf(`{"id":"t%04d","dir":"in","name":"x","peer_name":"p","files":1,"bytes":%d,"started":"2024-01-01T00:00:00Z","ended":"2024-01-01T00:00:00Z","result":"ok"}`+"\n", i, i)
		b.WriteString(s)
	}
	b.WriteString(`{"id":"torn","dir":`) // a write cut off mid-line
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := lines(t, s); n != Cap {
		t.Fatalf("after open: %d lines, want %d", n, Cap)
	}
	all, _ := s.List(0)
	if len(all) != Cap || all[0].ID != fmt.Sprintf("t%04d", Cap+249) || all[Cap-1].ID != "t0250" {
		t.Fatalf("kept %d entries, newest %s, oldest %s", len(all), all[0].ID, all[len(all)-1].ID)
	}

	// Appending past the cap compacts in-process too.
	for i := range Cap + 1 {
		if err := s.Add(entry(10_000 + i)); err != nil {
			t.Fatal(err)
		}
	}
	if n := lines(t, s); n != Cap {
		t.Fatalf("after %d adds: %d lines, want %d", Cap+1, n, Cap)
	}
	all, _ = s.List(0)
	if all[0].ID != fmt.Sprintf("t%04d", 10_000+Cap) {
		t.Fatalf("newest entry %s", all[0].ID)
	}

	// A small, clean file is left alone.
	s2, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = s2.Add(entry(1))
	if s3, err := Open(filepath.Dir(s2.Path())); err != nil || lines(t, s3) != 1 {
		t.Fatalf("reopen: %v", err)
	}
}
