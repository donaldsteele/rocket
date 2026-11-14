package peers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/node"
)

func TestApproveAndPersist(t *testing.T) {
	dir := t.TempDir()
	alice, err := node.Load(t.TempDir(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	fp, pub := alice.Fingerprint(), alice.PublicKeyB64()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.IsTrusted(fp, pub) {
		t.Fatal("empty store should trust nobody")
	}
	if err := s.Approve(fp, "alice", pub); err != nil {
		t.Fatal(err)
	}
	if !s.IsTrusted(fp, pub) {
		t.Fatal("approved fingerprint should be trusted")
	}

	// Reopen from disk: the approval must survive.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.IsTrusted(fp, pub) {
		t.Fatal("approval did not persist across reopen")
	}
	if got := s2.List(); len(got) != 1 || got[0].Name != "alice" || got[0].Pub != pub {
		t.Fatalf("unexpected list: %+v", got)
	}

	if err := s2.Revoke(fp); err != nil {
		t.Fatal(err)
	}
	if s2.IsTrusted(fp, pub) {
		t.Fatal("revoked fingerprint should not be trusted")
	}
}

func TestPubMismatchNotTrusted(t *testing.T) {
	s, _ := Open(t.TempDir())
	alice, _ := node.Load(t.TempDir(), "alice")
	mallory, _ := node.Load(t.TempDir(), "mallory")
	_ = s.Approve(alice.Fingerprint(), "alice", alice.PublicKeyB64())
	if s.IsTrusted(alice.Fingerprint(), mallory.PublicKeyB64()) {
		t.Fatal("trusted fingerprint with a different key must not be trusted")
	}
	if s.IsTrusted(alice.Fingerprint(), "") {
		t.Fatal("empty key must not be trusted")
	}
}

func TestMigrationDropsUnpinned(t *testing.T) {
	dir := t.TempDir()
	alice, _ := node.Load(t.TempDir(), "alice")
	old := `[{"fingerprint":"abcdef0123456789","name":"old","approved_at":"2026-01-01T00:00:00Z"},
{"fingerprint":"` + alice.Fingerprint() + `","name":"alice","pub":"` + alice.PublicKeyB64() + `","approved_at":"2026-01-01T00:00:00Z"},
{"fingerprint":"0000000000000000","name":"liar","pub":"` + alice.PublicKeyB64() + `","approved_at":"2026-01-01T00:00:00Z"}]`
	if err := os.WriteFile(filepath.Join(dir, "trusted.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.List(); len(got) != 1 || got[0].Name != "alice" {
		t.Fatalf("migration kept %+v, want only alice", got)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "trusted.json"))
	if strings.Contains(string(b), "abcdef0123456789") || strings.Contains(string(b), "liar") {
		t.Fatalf("migrated file still has dropped entries: %s", b)
	}
}

func TestDropTokens(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenDropTokens(dir)
	if err != nil {
		t.Fatal(err)
	}
	if d.Valid("") || d.Valid("nope") {
		t.Fatal("unknown token valid")
	}
	tok, err := d.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Valid(tok) {
		t.Fatal("issued token not valid")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "drop_tokens.json"))
	if strings.Contains(string(b), tok) {
		t.Fatal("token stored in clear")
	}
	if !strings.Contains(string(b), hashToken(tok)) {
		t.Fatal("token hash not stored")
	}
	d2, err := OpenDropTokens(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !d2.Valid(tok) {
		t.Fatal("token did not persist")
	}
	d2.Now = func() time.Time { return time.Now().Add(DropTokenTTL + time.Hour) }
	if d2.Valid(tok) {
		t.Fatal("expired token valid")
	}
}
