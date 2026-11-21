package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"192.168.1.20:29005":          true,
		"10.0.0.5:29005":              true,
		"172.16.4.1:29005":            true,
		"100.101.102.103:29005":       true, // Tailscale
		"100.127.255.254:29005":       true,
		"[fd7a:115c:a1e0::1]:29005":   true, // Tailscale IPv6 (ULA)
		"[fe80::1]:29005":             true,
		"100.128.0.1:29005":           false, // just outside 100.64.0.0/10
		"8.8.8.8:29005":               false,
		"[2001:4860:4860::8888]:443":  false,
		"127.0.0.1:29005":             false,
		"home-pc:29005":               false, // a host name can point anywhere
		"home-pc.tail1234.ts.net:443": false,
		"not an address":              false,
	} {
		if got := privateAddr(addr); got != want {
			t.Errorf("privateAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

// TestSendFileRefusesPublicAddress: an agent can send to a typed address
// only when it is a private IP, never a host name or an internet address.
func TestSendFileRefusesPublicAddress(t *testing.T) {
	cs := connect(t, testService(t))
	f := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, peer := range []string{"8.8.8.8", "8.8.8.8:29005", "example.com:29005"} {
		var sf sendFileOut
		call(t, cs, "send_file", map[string]any{"peer": peer, "paths": []string{f}}, &sf)
		if sf.Sent || !strings.Contains(sf.Note, "private IP") {
			t.Errorf("send_file to %s: %+v", peer, sf)
		}
	}
}
