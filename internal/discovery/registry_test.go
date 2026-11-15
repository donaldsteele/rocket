package discovery

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock for the registry.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const selfFP = "0000000000000000"

func newTestRegistry() (*Registry, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	r := New(Self{Name: "me", Fingerprint: selfFP, HTTPPort: 29005}, 29006)
	r.now = clk.now
	return r, clk
}

func onlyPeer(t *testing.T, r *Registry) Peer {
	t.Helper()
	ps := r.Peers()
	if len(ps) != 1 {
		t.Fatalf("got %d peers, want 1: %+v", len(ps), ps)
	}
	return ps[0]
}

func TestMergeBySourceTagsVia(t *testing.T) {
	r, _ := newTestRegistry()
	r.Observe(ViaMDNS, Peer{Name: "laptop", Fingerprint: "aaaaaaaaaaaaaaaa", Addr: "192.168.1.20:29005", HTTPPort: 29005, Version: "1.0.0", OS: "linux"})
	if got := onlyPeer(t, r).Via; !slices.Equal(got, []string{ViaMDNS}) {
		t.Fatalf("via = %v, want [mdns]", got)
	}
	r.Observe(ViaBeacon, Peer{Name: "laptop", Fingerprint: "aaaaaaaaaaaaaaaa", Addr: "192.168.1.20:29005", HTTPPort: 29005, Version: "1.0.0", OS: "linux"})
	p := onlyPeer(t, r)
	if !slices.Equal(p.Via, []string{ViaBeacon, ViaMDNS}) {
		t.Fatalf("via = %v, want [beacon mdns]", p.Via)
	}
	if p.Name != "laptop" || p.Addr != "192.168.1.20:29005" || p.OS != "linux" || p.Version != "1.0.0" {
		t.Fatalf("merged peer = %+v", p)
	}
}

func TestMergeBeaconSuppliesNameAndGapsFilled(t *testing.T) {
	r, _ := newTestRegistry()
	// mDNS renamed the instance after a conflict; the beacon carries the real name.
	r.Observe(ViaMDNS, Peer{Name: "laptop (2)", Fingerprint: "aaaaaaaaaaaaaaaa", Addr: "192.168.1.20:29005", HTTPPort: 29005, Version: "1.0.0", OS: "linux"})
	r.Observe(ViaBeacon, Peer{Name: "laptop", Fingerprint: "aaaaaaaaaaaaaaaa", Addr: "192.168.1.20:29005", HTTPPort: 29005})
	p := onlyPeer(t, r)
	if p.Name != "laptop" {
		t.Fatalf("name = %q, want the beacon's", p.Name)
	}
	if p.OS != "linux" || p.Version != "1.0.0" {
		t.Fatalf("gaps not filled from mdns: %+v", p)
	}
}

func TestMergePrefersIPv4Address(t *testing.T) {
	r, _ := newTestRegistry()
	// Highest-priority source has only an IPv6 address; a lower one has IPv4.
	r.Observe(ViaBeacon, Peer{Name: "x", Fingerprint: "bbbbbbbbbbbbbbbb", Addr: "[2001:db8::5]:29005", HTTPPort: 29005})
	r.Observe(ViaMDNS, Peer{Name: "x", Fingerprint: "bbbbbbbbbbbbbbbb", Addr: "10.0.0.5:29010", HTTPPort: 29010})
	p := onlyPeer(t, r)
	if p.Addr != "10.0.0.5:29010" || p.HTTPPort != 29010 {
		t.Fatalf("addr = %q port %d, want the IPv4 one", p.Addr, p.HTTPPort)
	}

	// With only IPv6 available, it is used.
	r2, _ := newTestRegistry()
	r2.Observe(ViaMDNS, Peer{Name: "y", Fingerprint: "cccccccccccccccc", Addr: "[2001:db8::7]:29005", HTTPPort: 29005})
	if p := onlyPeer(t, r2); p.Addr != "[2001:db8::7]:29005" {
		t.Fatalf("addr = %q, want the IPv6 one", p.Addr)
	}
}

func TestExpiryPerSource(t *testing.T) {
	r, clk := newTestRegistry()
	fp := "dddddddddddddddd"
	r.Observe(ViaBeacon, Peer{Name: "d", Fingerprint: fp, Addr: "192.168.1.9:29005", HTTPPort: 29005})
	r.Observe(ViaMDNS, Peer{Name: "d", Fingerprint: fp, Addr: "192.168.1.9:29005", HTTPPort: 29005})

	clk.advance(peerTTL + time.Second) // beacon sighting stale, mdns still live
	p := onlyPeer(t, r)
	if !slices.Equal(p.Via, []string{ViaMDNS}) {
		t.Fatalf("via after beacon expiry = %v, want [mdns]", p.Via)
	}

	clk.advance(mdnsTTL) // now both are stale
	if ps := r.Peers(); len(ps) != 0 {
		t.Fatalf("peers after full expiry = %+v, want none", ps)
	}
	r.mu.Lock()
	n := len(r.peers)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("expired entries kept in the map: %d", n)
	}
}

func TestRefreshKeepsPeerLive(t *testing.T) {
	r, clk := newTestRegistry()
	fp := "eeeeeeeeeeeeeeee"
	for i := 0; i < 10; i++ {
		r.Observe(ViaBeacon, Peer{Name: "e", Fingerprint: fp, Addr: "192.168.1.3:29005", HTTPPort: 29005})
		clk.advance(announceInterval)
	}
	if p := onlyPeer(t, r); !slices.Equal(p.Via, []string{ViaBeacon}) {
		t.Fatalf("via = %v", p.Via)
	}
}

func TestForgetDropsOnlyThatSource(t *testing.T) {
	r, _ := newTestRegistry()
	fp := "ffffffffffffffff"
	r.Observe(ViaBeacon, Peer{Name: "f", Fingerprint: fp, Addr: "192.168.1.4:29005", HTTPPort: 29005})
	r.Observe(ViaMDNS, Peer{Name: "f", Fingerprint: fp, Addr: "192.168.1.4:29005", HTTPPort: 29005})
	r.forget(ViaMDNS, fp)
	if p := onlyPeer(t, r); !slices.Equal(p.Via, []string{ViaBeacon}) {
		t.Fatalf("via after mdns goodbye = %v, want [beacon]", p.Via)
	}
	r.forget(ViaBeacon, fp)
	if ps := r.Peers(); len(ps) != 0 {
		t.Fatalf("peers = %+v, want none", ps)
	}
	r.forget(ViaMDNS, "1234567890abcdef") // unknown: no panic
}

func TestSelfAndBlankIgnored(t *testing.T) {
	r, _ := newTestRegistry()
	r.Observe(ViaMDNS, Peer{Name: "me", Fingerprint: selfFP, Addr: "192.168.1.2:29005"})
	r.Observe(ViaBeacon, Peer{Name: "nofp", Addr: "192.168.1.2:29005"})
	if ps := r.Peers(); len(ps) != 0 {
		t.Fatalf("peers = %+v, want none", ps)
	}
}

func TestPeersFreshestFirstAndFind(t *testing.T) {
	r, clk := newTestRegistry()
	r.Observe(ViaBeacon, Peer{Name: "Old", Fingerprint: "1111111111111111", Addr: "192.168.1.5:29005"})
	clk.advance(time.Second)
	r.Observe(ViaMDNS, Peer{Name: "New", Fingerprint: "2222222222222222", Addr: "192.168.1.6:29005"})
	ps := r.Peers()
	if len(ps) != 2 || ps[0].Name != "New" || ps[1].Name != "Old" {
		t.Fatalf("order = %+v", ps)
	}
	if p, ok := r.Find("old"); !ok || p.Fingerprint != "1111111111111111" {
		t.Fatalf("Find by name = %+v %v", p, ok)
	}
	if p, ok := r.Find("2222"); !ok || p.Name != "New" {
		t.Fatalf("Find by fp prefix = %+v %v", p, ok)
	}
	if _, ok := r.Find(""); ok {
		t.Fatal("Find(\"\") matched")
	}
}

// TestResolveExactFingerprintBeatsNames covers a LAN host that names itself
// after another peer's fingerprint and announces more often: a full
// fingerprint must still resolve to the real peer, never to the impostor.
func TestResolveExactFingerprintBeatsNames(t *testing.T) {
	r, clk := newTestRegistry()
	const victim, attacker = "ab12cd34ef567890", "9999999999999999"
	r.Observe(ViaBeacon, Peer{Name: "laptop", Fingerprint: victim, Addr: "192.168.1.5:29005"})
	clk.advance(time.Second)
	r.Observe(ViaBeacon, Peer{Name: victim, Fingerprint: attacker, Addr: "192.168.1.66:29005"})
	p, err := r.Resolve(victim)
	if err != nil || p.Fingerprint != victim {
		t.Fatalf("Resolve(full fp) = %+v, %v; want the victim", p, err)
	}
	if p, err := r.Resolve(strings.ToUpper(victim)); err != nil || p.Fingerprint != victim {
		t.Fatalf("Resolve(upper-case fp) = %+v, %v", p, err)
	}
	// With the real peer gone, the impostor's name still does not match a
	// full fingerprint.
	clk.advance(peerTTL)
	r.Observe(ViaBeacon, Peer{Name: victim, Fingerprint: attacker, Addr: "192.168.1.66:29005"})
	if p, err := r.Resolve(victim); !errors.Is(err, ErrPeerNotFound) {
		t.Fatalf("Resolve(fp of a gone peer) = %+v, %v; want not found", p, err)
	}
}

func TestResolveAmbiguous(t *testing.T) {
	r, _ := newTestRegistry()
	r.Observe(ViaBeacon, Peer{Name: "desk", Fingerprint: "ab12000000000001", Addr: "192.168.1.5:29005"})
	r.Observe(ViaBeacon, Peer{Name: "desk", Fingerprint: "cd34000000000002", Addr: "192.168.1.6:29005"})
	r.Observe(ViaBeacon, Peer{Name: "ab12", Fingerprint: "ef56000000000003", Addr: "192.168.1.7:29005"})
	for _, target := range []string{"desk", "DESK", "ab12"} {
		if p, err := r.Resolve(target); !errors.Is(err, ErrPeerAmbiguous) {
			t.Errorf("Resolve(%q) = %+v, %v; want ambiguous", target, p, err)
		}
		if _, ok := r.Find(target); ok {
			t.Errorf("Find(%q) matched an ambiguous target", target)
		}
	}
	if p, err := r.Resolve("cd34"); err != nil || p.Fingerprint != "cd34000000000002" {
		t.Fatalf("Resolve(unique prefix) = %+v, %v", p, err)
	}
	if _, err := r.Resolve("nobody"); !errors.Is(err, ErrPeerNotFound) {
		t.Fatalf("Resolve(unknown) = %v", err)
	}
}

func TestPeersReturnsCopies(t *testing.T) {
	r, _ := newTestRegistry()
	r.Observe(ViaBeacon, Peer{Name: "c", Fingerprint: "3333333333333333", Addr: "192.168.1.7:29005"})
	p := onlyPeer(t, r)
	p.Via[0] = "tampered"
	if got := onlyPeer(t, r).Via; got[0] != ViaBeacon {
		t.Fatalf("Peers shares Via with the registry: %v", got)
	}
}

func TestRegistryBounded(t *testing.T) {
	r, _ := newTestRegistry()
	for i := 0; i < maxPeers+50; i++ {
		r.Observe(ViaBeacon, Peer{Name: "n", Fingerprint: fmt.Sprintf("%016x", i+1), Addr: "192.168.1.8:29005"})
	}
	if n := len(r.Peers()); n != maxPeers {
		t.Fatalf("registry holds %d peers, want cap %d", n, maxPeers)
	}
}

func TestBeaconPeer(t *testing.T) {
	src := &net.UDPAddr{IP: net.ParseIP("192.168.1.30"), Port: 29006}
	p, ok := beaconPeer([]byte(`{"name":"desk","fp":"abcdefabcdefabcd","port":29005,"ver":"1.0.0","os":"windows"}`), src)
	if !ok || p.Addr != "192.168.1.30:29005" || p.Name != "desk" || p.OS != "windows" || p.Version != "1.0.0" {
		t.Fatalf("beaconPeer = %+v %v", p, ok)
	}
	for _, bad := range []string{
		`not json`, `{"name":"x"}`, `{"fp":"abcdefabcdefabcd","port":0}`, `{"fp":"abcdefabcdefabcd","port":70000}`,
		`{"fp":"abcd","port":29005}`, `{"fp":"ABCDEFABCDEFABCD","port":29005}`, `{"fp":"abcdefabcdefabcz","port":29005}`,
	} {
		if _, ok := beaconPeer([]byte(bad), src); ok {
			t.Errorf("beaconPeer(%s) accepted", bad)
		}
	}
}
