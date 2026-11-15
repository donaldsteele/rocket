package discovery

import (
	"net"
	"slices"
	"testing"

	"github.com/brutella/dnssd"
)

func TestTXT(t *testing.T) {
	got := TXT(Self{Name: "desk", Fingerprint: "abcdefabcdefabcd", HTTPPort: 29005, Version: "1.1.0", OS: "windows"})
	want := map[string]string{"fp": "abcdefabcdefabcd", "ver": "1.1.0", "os": "windows", "proto": "2"}
	if len(got) != len(want) {
		t.Fatalf("TXT = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("TXT[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func entry(ips []string, port int, txt map[string]string) dnssd.BrowseEntry {
	e := dnssd.BrowseEntry{Name: "laptop", Type: ServiceType, Domain: "local", Port: port, Text: txt}
	for _, s := range ips {
		e.IPs = append(e.IPs, net.ParseIP(s))
	}
	return e
}

func goodTXT() map[string]string {
	return map[string]string{"fp": "abcdefabcdefabcd", "ver": "1.1.0", "os": "linux", "proto": "2"}
}

func TestEntryPeerPrefersIPv4(t *testing.T) {
	p, ok := entryPeer(entry([]string{"fe80::1", "2001:db8::9", "192.168.1.40"}, 29005, goodTXT()))
	if !ok {
		t.Fatal("entry rejected")
	}
	if p.Addr != "192.168.1.40:29005" || p.Fingerprint != "abcdefabcdefabcd" || p.Name != "laptop" ||
		p.Version != "1.1.0" || p.OS != "linux" || p.HTTPPort != 29005 || p.Proto != 2 {
		t.Fatalf("peer = %+v", p)
	}

	p, ok = entryPeer(entry([]string{"169.254.10.1", "10.1.2.3"}, 29005, goodTXT()))
	if !ok || p.Addr != "10.1.2.3:29005" {
		t.Fatalf("link-local IPv4 not deprioritised: %+v %v", p, ok)
	}
	p, ok = entryPeer(entry([]string{"2001:db8::9", "169.254.10.1"}, 29005, goodTXT()))
	if !ok || p.Addr != "169.254.10.1:29005" {
		t.Fatalf("link-local IPv4 should still beat IPv6: %+v %v", p, ok)
	}

	p, ok = entryPeer(entry([]string{"fe80::1", "2001:db8::9"}, 29005, goodTXT()))
	if !ok || p.Addr != "[2001:db8::9]:29005" {
		t.Fatalf("IPv6 fallback = %+v %v", p, ok)
	}
}

func TestEntryPeerRejects(t *testing.T) {
	cases := map[string]dnssd.BrowseEntry{
		"no fp":          entry([]string{"192.168.1.40"}, 29005, map[string]string{"proto": "1"}),
		"upper-case fp":  entry([]string{"192.168.1.40"}, 29005, map[string]string{"fp": "ABCDEFABCDEFABCD", "proto": "1"}),
		"short fp":       entry([]string{"192.168.1.40"}, 29005, map[string]string{"fp": "abcdef", "proto": "1"}),
		"zero port":      entry([]string{"192.168.1.40"}, 0, goodTXT()),
		"link-local v6":  entry([]string{"fe80::1"}, 29005, goodTXT()),
		"no addresses":   entry(nil, 29005, goodTXT()),
		"unspecified v4": entry([]string{"0.0.0.0"}, 29005, goodTXT()),
	}
	for name, e := range cases {
		if p, ok := entryPeer(e); ok {
			t.Errorf("%s: accepted as %+v", name, p)
		}
	}
}

// TestEntryPeerProto: older and unknown versions are listed (so they can be
// shown as needing an update), with a missing or unreadable version as 1.
func TestEntryPeerProto(t *testing.T) {
	for txt, want := range map[string]int{"2": 2, "1": 1, "": 1, "x": 1, "-3": 1, "3": 3} {
		text := map[string]string{"fp": "abcdefabcdefabcd"}
		if txt != "" {
			text["proto"] = txt
		}
		p, ok := entryPeer(entry([]string{"192.168.1.40"}, 29005, text))
		if !ok || p.Proto != want {
			t.Errorf("proto %q: %+v %v, want %d", txt, p, ok, want)
		}
	}
}

func TestMDNSCallbacksFeedRegistry(t *testing.T) {
	r, _ := newTestRegistry()
	e := entry([]string{"192.168.1.40"}, 29005, goodTXT())
	r.mdnsAdd(e)
	p := onlyPeer(t, r)
	if !slices.Equal(p.Via, []string{ViaMDNS}) || p.Addr != "192.168.1.40:29005" {
		t.Fatalf("peer = %+v", p)
	}
	r.mdnsRemove(e)
	if ps := r.Peers(); len(ps) != 0 {
		t.Fatalf("peers after goodbye = %+v", ps)
	}

	// Our own advertisement, seen by our own browser, is ignored.
	own := entry([]string{"192.168.1.2"}, 29005, map[string]string{"fp": selfFP, "proto": "1"})
	r.mdnsAdd(own)
	if ps := r.Peers(); len(ps) != 0 {
		t.Fatalf("own advertisement listed: %+v", ps)
	}
}

func TestRegistryAdvertiserOptions(t *testing.T) {
	r := New(Self{Name: "desk", Fingerprint: "abcdefabcdefabcd", HTTPPort: 29105, Version: "1.0.0", OS: "windows"}, 29106)
	a := r.Advertiser(nil)
	if a.opts.Instance != "desk" || a.opts.Host != "rocket-abcdefab" || a.opts.Port != 29105 {
		t.Fatalf("options = %+v", a.opts)
	}
	if a.opts.Text["fp"] != "abcdefabcdefabcd" || a.opts.Text["proto"] != TXTProto {
		t.Fatalf("text = %v", a.opts.Text)
	}
	a.Close() // never started: no-op
}
