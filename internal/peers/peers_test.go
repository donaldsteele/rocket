package peers

import "testing"

func TestApproveAndPersist(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.IsTrusted("abc") {
		t.Fatal("empty store should trust nobody")
	}
	if err := s.Approve("abc", "alice"); err != nil {
		t.Fatal(err)
	}
	if !s.IsTrusted("abc") {
		t.Fatal("approved fingerprint should be trusted")
	}

	// Reopen from disk: the approval must survive.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.IsTrusted("abc") {
		t.Fatal("approval did not persist across reopen")
	}
	if got := s2.List(); len(got) != 1 || got[0].Name != "alice" {
		t.Fatalf("unexpected list: %+v", got)
	}

	if err := s2.Revoke("abc"); err != nil {
		t.Fatal(err)
	}
	if s2.IsTrusted("abc") {
		t.Fatal("revoked fingerprint should not be trusted")
	}
}
