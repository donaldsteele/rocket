package resume

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDir(t *testing.T) {
	d, err := OpenDir(filepath.Join(t.TempDir(), "recs"))
	if err != nil {
		t.Fatal(err)
	}
	id := "00112233445566778899aabbccddeeff"
	if err := d.Save(id, map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	var v map[string]int
	if err := d.Load(id, &v); err != nil || v["n"] != 1 {
		t.Fatalf("load %v %v", v, err)
	}
	for _, bad := range []string{"../evil", "short", "a/b0123456789abcdef", ""} {
		if err := d.Save(bad, v); !errors.Is(err, ErrBadID) {
			t.Errorf("save %q: %v", bad, err)
		}
	}
	_ = os.WriteFile(filepath.Join(d.Path(), "notes.txt"), []byte("x"), 0o600)
	if ids, _ := d.IDs(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("ids %v", ids)
	}
	if err := d.Remove(id); err != nil {
		t.Fatal(err)
	}
	if err := d.Load(id, &v); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("load after remove: %v", err)
	}
	if err := d.Remove(id); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

func TestOutboxExpiry(t *testing.T) {
	b, err := OpenOutbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	b.Now = func() time.Time { return now }
	a := Outgoing{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "a"}
	if err := b.Put(a); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := b.Put(Outgoing{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Name: "b"}); err != nil {
		t.Fatal(err)
	}
	if l := b.List(); len(l) != 2 || l[0].Name != "b" || !l[1].Expires.Equal(l[1].Interrupted.Add(KeepFor)) {
		t.Fatalf("list %+v", l)
	}
	now = now.Add(KeepFor - 30*time.Minute) // a is past 24 h, b not
	if _, ok := b.Get(a.ID); ok {
		t.Fatal("expired record returned")
	}
	if n := b.Sweep(); n != 1 {
		t.Fatalf("swept %d", n)
	}
	if l := b.List(); len(l) != 1 || l[0].Name != "b" {
		t.Fatalf("after sweep %+v", l)
	}
}

func TestOutboxSupersede(t *testing.T) {
	b, err := OpenOutbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	recs := []Outgoing{
		{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ManifestHash: "h1", PeerFP: "0123456789abcdef", Addr: "10.0.0.2:29005"},
		{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ManifestHash: "h1", PeerFP: "fedcba9876543210", Addr: "10.0.0.3:29005"},
		{ID: "cccccccccccccccccccccccccccccccc", ManifestHash: "h2", PeerFP: "0123456789abcdef", Addr: "10.0.0.2:29005"},
		{ID: "dddddddddddddddddddddddddddddddd", ManifestHash: "h1", Addr: "10.0.0.4:29005"},
	}
	for _, r := range recs {
		if err := b.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	// Same files, same peer (it may have a new address): gone.
	if n := b.Supersede("h1", "0123456789abcdef", "10.0.0.9:29005"); n != 1 {
		t.Fatalf("superseded %d, want 1", n)
	}
	if _, ok := b.Get(recs[0].ID); ok {
		t.Fatal("matching record kept")
	}
	// A bare host:port send matches by address only.
	if n := b.Supersede("h1", "", "10.0.0.3:29005"); n != 0 {
		t.Fatalf("superseded %d by address of a fingerprinted record", n)
	}
	if n := b.Supersede("h1", "", "10.0.0.4:29005"); n != 1 {
		t.Fatalf("superseded %d by address, want 1", n)
	}
	if n := b.Supersede("", "", ""); n != 0 {
		t.Fatalf("empty hash superseded %d", n)
	}
	if l := b.List(); len(l) != 2 {
		t.Fatalf("left %+v", l)
	}
}
