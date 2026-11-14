// Package discovery finds other rocket hosts on the LAN.
//
// Two sources feed one Registry keyed by fingerprint: a small UDP broadcast
// beacon (beacon.go) and mDNS/DNS-SD on _rocket._tcp.local (mdns.go). Each
// source reports sightings; the registry merges them per peer, tags the peer
// with the sources that currently see it (Via) and expires sightings that stop
// arriving.
package discovery

import (
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// Discovery sources, as reported in Peer.Via.
const (
	ViaBeacon = "beacon"
	ViaMDNS   = "mdns"
)

const (
	// peerTTL is how long a beacon sighting stays live (beacons repeat every
	// announceInterval).
	peerTTL = 7 * time.Second
	// mdnsTTL is how long an mDNS sighting stays live. Browsing restarts every
	// browseInterval, which refreshes every peer that still answers.
	mdnsTTL = 75 * time.Second
	// maxPeers bounds the registry so a flood of forged announcements cannot
	// grow it without limit.
	maxPeers = 1024
)

// sources lists the discovery sources in merge priority order: the first live
// one supplies the peer's name, version and OS.
var sources = []string{ViaBeacon, ViaMDNS}

func ttlFor(via string) time.Duration {
	if via == ViaMDNS {
		return mdnsTTL
	}
	return peerTTL
}

// Peer is another rocket host seen on the network.
type Peer struct {
	Name        string    `json:"name"`
	Fingerprint string    `json:"fp"`
	Addr        string    `json:"-"` // host:port for the HTTP API
	HTTPPort    int       `json:"port"`
	Version     string    `json:"ver"`
	OS          string    `json:"os"`
	Via         []string  `json:"via"` // live sources: "beacon", "mdns"
	LastSeen    time.Time `json:"-"`
}

// Self describes this host, announced to others.
type Self struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fp"`
	HTTPPort    int    `json:"port"`
	Version     string `json:"ver"`
	OS          string `json:"os"`
}

// sighting is one source's latest report of a peer.
type sighting struct {
	peer Peer
	seen time.Time
}

// Registry advertises this host and tracks discovered peers.
type Registry struct {
	self       Self
	beaconPort int
	now        func() time.Time // replaced in tests

	mu         sync.Mutex
	peers      map[string]map[string]sighting // fingerprint -> source -> sighting
	lastExpire time.Time
}

// New creates a registry for this host. beaconPort is the UDP port Run uses.
func New(self Self, beaconPort int) *Registry {
	return &Registry{
		self: self, beaconPort: beaconPort, now: time.Now,
		peers: map[string]map[string]sighting{},
	}
}

// Self returns the description of this host the registry announces.
func (r *Registry) Self() Self { return r.self }

// Observe records that source via (ViaBeacon, ViaMDNS) saw p now. The beacon
// and mDNS browsers call it; tests use it to seed peers. Sightings of this
// host, or without a fingerprint, are ignored.
func (r *Registry) Observe(via string, p Peer) {
	if p.Fingerprint == "" || p.Fingerprint == r.self.Fingerprint {
		return
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Sub(r.lastExpire) >= peerTTL {
		r.expireLocked(now)
	}
	byVia, ok := r.peers[p.Fingerprint]
	if !ok {
		if len(r.peers) >= maxPeers {
			return
		}
		byVia = map[string]sighting{}
		r.peers[p.Fingerprint] = byVia
	}
	p.Via, p.LastSeen = nil, time.Time{}
	byVia[via] = sighting{peer: p, seen: now}
}

// forget drops source via's sighting of the peer with fingerprint fp (an mDNS
// goodbye, for example). Other sources' sightings are kept.
func (r *Registry) forget(via, fp string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if byVia, ok := r.peers[fp]; ok {
		delete(byVia, via)
		if len(byVia) == 0 {
			delete(r.peers, fp)
		}
	}
}

// expireLocked deletes sightings older than their source's TTL.
func (r *Registry) expireLocked(now time.Time) {
	r.lastExpire = now
	for fp, byVia := range r.peers {
		for via, s := range byVia {
			if now.Sub(s.seen) > ttlFor(via) {
				delete(byVia, via)
			}
		}
		if len(byVia) == 0 {
			delete(r.peers, fp)
		}
	}
}

// Peers returns the currently live peers (self excluded), freshest first.
func (r *Registry) Peers() []Peer {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
	out := make([]Peer, 0, len(r.peers))
	for _, byVia := range r.peers {
		if p, ok := merge(byVia); ok {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Peer) int {
		if c := b.LastSeen.Compare(a.LastSeen); c != 0 {
			return c
		}
		return strings.Compare(a.Fingerprint, b.Fingerprint)
	})
	return out
}

// merge combines one peer's live sightings. The highest-priority source
// supplies the name, version and OS (gaps filled from the others); the address
// is the first IPv4 one in priority order, else the primary source's.
func merge(byVia map[string]sighting) (Peer, bool) {
	var (
		out   Peer
		found bool
		v4    bool
	)
	for _, via := range sources {
		s, ok := byVia[via]
		if !ok {
			continue
		}
		p := s.peer
		if !found {
			out, found = p, true
			out.Via = nil
			v4 = isIPv4Addr(p.Addr)
		} else {
			if out.Name == "" {
				out.Name = p.Name
			}
			if out.Version == "" {
				out.Version = p.Version
			}
			if out.OS == "" {
				out.OS = p.OS
			}
			if !v4 && isIPv4Addr(p.Addr) {
				out.Addr, out.HTTPPort, v4 = p.Addr, p.HTTPPort, true
			}
		}
		out.Via = append(out.Via, via)
		if s.seen.After(out.LastSeen) {
			out.LastSeen = s.seen
		}
	}
	return out, found
}

// isIPv4Addr reports whether the host:port addr has an IPv4 host.
func isIPv4Addr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() != nil
}

// Find returns the live peer whose name matches target (case-insensitive) or
// whose fingerprint starts with target.
func (r *Registry) Find(target string) (Peer, bool) {
	if target == "" {
		return Peer{}, false
	}
	low := strings.ToLower(target)
	for _, p := range r.Peers() {
		if strings.EqualFold(p.Name, target) || strings.HasPrefix(p.Fingerprint, low) {
			return p, true
		}
	}
	return Peer{}, false
}
