package transfer

import "testing"

func TestAddress(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"192.168.1.20:29005", "192.168.1.20:29005", true},
		{"home-pc:29010", "home-pc:29010", true},
		{"home-pc.tail1234.ts.net:29005", "home-pc.tail1234.ts.net:29005", true},
		{"100.101.102.103", "100.101.102.103:29005", true},
		{"fd7a:115c:a1e0::1", "[fd7a:115c:a1e0::1]:29005", true},
		{"[fd7a:115c:a1e0::1]", "[fd7a:115c:a1e0::1]:29005", true},
		{"[fd7a:115c:a1e0::1]:4000", "[fd7a:115c:a1e0::1]:4000", true},
		{"home-pc", "", false},           // could be a peer name
		{"cfe4dd4a", "", false},          // a fingerprint prefix
		{"192.168.1.20:0", "", false},    // no such port
		{"192.168.1.20:http", "", false}, // a numeric port only
		{"fe80::1%eth0", "", false},
		{"", "", false},
	} {
		got, ok := Address(c.in, 29005)
		if got != c.want || ok != c.ok {
			t.Errorf("Address(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
