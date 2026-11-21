package peers

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/node"
)

// TestAddByKey: a sender added by public key is trusted for that key only.
func TestAddByKey(t *testing.T) {
	s, _ := Open(t.TempDir())
	alice, _ := node.Load(t.TempDir(), "alice")
	mallory, _ := node.Load(t.TempDir(), "mallory")
	got, err := s.Add("", "alice", alice.PublicKeyB64())
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != alice.Fingerprint() || got.ByFingerprint {
		t.Fatalf("stored %+v", got)
	}
	if !s.IsTrusted(alice.Fingerprint(), alice.PublicKeyB64()) {
		t.Fatal("alice not trusted")
	}
	if s.IsTrusted(mallory.Fingerprint(), mallory.PublicKeyB64()) {
		t.Fatal("mallory trusted")
	}
	if _, err := s.Add(mallory.Fingerprint(), "x", alice.PublicKeyB64()); err != ErrKeyMismatch {
		t.Fatalf("key and fingerprint of different hosts: %v", err)
	}
	if _, err := s.Add("", "x", "not-a-key"); err == nil {
		t.Fatal("garbage key accepted")
	}
}

// TestAddByFingerprintPinsOnFirstUse: a fingerprint entry trusts a key
// that derives to it, then Pin makes it a key entry.
func TestAddByFingerprintPinsOnFirstUse(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	alice, _ := node.Load(t.TempDir(), "alice")
	mallory, _ := node.Load(t.TempDir(), "mallory")
	if _, err := s.Add("ABC", "", ""); err == nil {
		t.Fatal("bad fingerprint accepted")
	}
	got, err := s.Add(alice.Fingerprint(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ByFingerprint || got.Pub != "" {
		t.Fatalf("stored %+v", got)
	}
	// Mallory claims alice's fingerprint with her own key: the verifier
	// would refuse the offer, and the store does not trust it either.
	if s.IsTrusted(alice.Fingerprint(), mallory.PublicKeyB64()) {
		t.Fatal("a key that does not derive to the fingerprint was trusted")
	}
	if !s.IsTrusted(alice.Fingerprint(), alice.PublicKeyB64()) {
		t.Fatal("alice's key not trusted by fingerprint")
	}
	if err := s.Pin(alice.Fingerprint(), "alice", mallory.PublicKeyB64()); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get(alice.Fingerprint()); e.Pub != "" {
		t.Fatal("pinned a key that does not derive to the fingerprint")
	}
	if err := s.Pin(alice.Fingerprint(), "alice", alice.PublicKeyB64()); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Get(alice.Fingerprint())
	if e.Pub != alice.PublicKeyB64() || e.ByFingerprint || e.Name != "alice" {
		t.Fatalf("after pin: %+v", e)
	}
	// It survives a reopen as a key entry.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := s2.Get(alice.Fingerprint()); e.Pub != alice.PublicKeyB64() {
		t.Fatalf("reopened: %+v", e)
	}
	// Adding the fingerprint again keeps the pinned key.
	if got, _ := s2.Add(alice.Fingerprint(), "renamed", ""); got.Pub != alice.PublicKeyB64() || got.Name != "renamed" {
		t.Fatalf("re-add: %+v", got)
	}
}

// TestSharedFile: two stores on one data folder (the UI and the command
// line) see each other's changes and do not overwrite them.
func TestSharedFile(t *testing.T) {
	dir := t.TempDir()
	ui, _ := Open(dir)
	cli, _ := Open(dir)
	alice, _ := node.Load(t.TempDir(), "alice")
	bob, _ := node.Load(t.TempDir(), "bob")
	if _, err := cli.Add("", "alice", alice.PublicKeyB64()); err != nil {
		t.Fatal(err)
	}
	if !ui.IsTrusted(alice.Fingerprint(), alice.PublicKeyB64()) {
		t.Fatal("the UI's store did not see the command line's add")
	}
	// Make sure the next write gets a new modification time.
	future := time.Now().Add(2 * time.Second)
	if err := ui.Approve(bob.Fingerprint(), "bob", bob.PublicKeyB64()); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filepath.Join(dir, "trusted.json"), future, future)
	if len(cli.List()) != 2 {
		t.Fatalf("command line sees %+v", cli.List())
	}
	if err := cli.Revoke(alice.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	if ui.IsTrusted(alice.Fingerprint(), alice.PublicKeyB64()) || !ui.IsTrusted(bob.Fingerprint(), bob.PublicKeyB64()) {
		t.Fatalf("UI after revoke: %+v", ui.List())
	}
}

// TestUnmarkedUnpinnedStillDropped: an entry with no key and no
// by_fingerprint mark is not trusted (the old migration rule holds).
func TestUnmarkedUnpinnedStillDropped(t *testing.T) {
	if Valid(Trusted{Fingerprint: "abcdef0123456789"}) {
		t.Fatal("unmarked entry without a key is valid")
	}
	if !Valid(Trusted{Fingerprint: "abcdef0123456789", ByFingerprint: true}) {
		t.Fatal("fingerprint entry is not valid")
	}
}
