package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/peers"
)

func trustCmd(t *testing.T, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	code = runTrustTo(args, &o, &e)
	return code, o.String(), e.String()
}

func TestTrustCommand(t *testing.T) {
	dir := t.TempDir()
	alice, _ := node.Load(t.TempDir(), "alice")
	bob, _ := node.Load(t.TempDir(), "bob")

	if code, out, _ := trustCmd(t, "--data-dir", dir); code != 0 || !strings.Contains(out, "no trusted senders") {
		t.Fatalf("empty list: %d %q", code, out)
	}
	// By key, with flags after the argument.
	if code, out, e := trustCmd(t, "add", alice.PublicKeyB64(), "--name", "alice", "--data-dir", dir); code != 0 || !strings.Contains(out, "pinned to its public key") {
		t.Fatalf("add key: %d %q %q", code, out, e)
	}
	// By fingerprint, upper case accepted.
	if code, out, e := trustCmd(t, "add", "--data-dir", dir, strings.ToUpper(bob.Fingerprint())); code != 0 || !strings.Contains(out, "by fingerprint") {
		t.Fatalf("add fingerprint: %d %q %q", code, out, e)
	}
	if code, _, e := trustCmd(t, "add", "--data-dir", dir, "nonsense"); code != 1 || !strings.Contains(e, "not a public key") {
		t.Fatalf("add garbage: %d %q", code, e)
	}
	code, out, _ := trustCmd(t, "list", "--data-dir", dir)
	if code != 0 || !strings.Contains(out, alice.Fingerprint()) || !strings.Contains(out, bob.Fingerprint()) || !strings.Contains(out, "key on first offer") {
		t.Fatalf("list: %d %q", code, out)
	}
	s, _ := peers.Open(dir)
	if !s.IsTrusted(alice.Fingerprint(), alice.PublicKeyB64()) || !s.IsTrusted(bob.Fingerprint(), bob.PublicKeyB64()) {
		t.Fatalf("store: %+v", s.List())
	}
	if code, out, _ := trustCmd(t, "remove", alice.Fingerprint(), "--data-dir", dir); code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("remove: %d %q", code, out)
	}
	if code, _, e := trustCmd(t, "remove", alice.Fingerprint(), "--data-dir", dir); code != 1 || !strings.Contains(e, "not a trusted sender") {
		t.Fatalf("remove twice: %d %q", code, e)
	}
	if code, _, _ := trustCmd(t, "frobnicate", "--data-dir", dir); code != 2 {
		t.Fatalf("unknown subcommand: %d", code)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"Studio-PC": "Studio-PC", "Don's Mac": `"Don's Mac"`, "": `""`} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
