// Package discovery finds other rocket hosts on the LAN.
//
// It uses a small UDP broadcast beacon: each host periodically broadcasts a
// JSON announcement, and every host listens and keeps a live peer list,
// expiring entries that stop announcing. This is stdlib-only and works on a
// flat LAN. (mDNS/_rocket._tcp can replace this later without touching callers.)
package discovery

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	announceInterval = 2 * time.Second
	peerTTL          = 7 * time.Second
	maxPacket        = 2048
)

// Peer is another rocket host seen on the network.
type Peer struct {
	Name        string    `json:"name"`
	Fingerprint string    `json:"fp"`
	Addr        string    `json:"-"` // host:port for the HTTP API, filled from the packet source
	HTTPPort    int       `json:"port"`
	Version     string    `json:"ver"`
	OS          string    `json:"os"`
	LastSeen    time.Time `json:"-"`
}

// Self describes this host, broadcast to others.
type Self struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fp"`
	HTTPPort    int    `json:"port"`
	Version     string `json:"ver"`
	OS          string `json:"os"`
}

// Registry advertises this host and tracks discovered peers.
type Registry struct {
	self       Self
	beaconPort int
	mu         sync.RWMutex
	peers      map[string]Peer // keyed by fingerprint
}

// New creates a registry for this host.
func New(self Self, beaconPort int) *Registry {
	return &Registry{self: self, beaconPort: beaconPort, peers: map[string]Peer{}}
}

// Peers returns the currently live peers (self excluded), freshest first.
func (r *Registry) Peers() []Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Peer, 0, len(r.peers))
	cutoff := time.Now().Add(-peerTTL)
	for _, p := range r.peers {
		if p.LastSeen.After(cutoff) {
			out = append(out, p)
		}
	}
	return out
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

// Run advertises this host and listens for peers until ctx is cancelled.
func (r *Registry) Run(ctx context.Context) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: r.beaconPort})
	if err != nil {
		return err
	}
	go r.listen(ctx, conn)
	go r.announce(ctx)
	go r.reap(ctx)
	<-ctx.Done()
	return conn.Close()
}

func (r *Registry) announce(ctx context.Context) {
	bcast := &net.UDPAddr{IP: net.IPv4bcast, Port: r.beaconPort}
	out, err := net.DialUDP("udp4", nil, bcast)
	if err != nil {
		return
	}
	defer out.Close()
	payload, _ := json.Marshal(r.self)
	t := time.NewTicker(announceInterval)
	defer t.Stop()
	_, _ = out.Write(payload) // announce immediately
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = out.Write(payload)
		}
	}
}

func (r *Registry) listen(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, maxPacket)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		var s Self
		if json.Unmarshal(buf[:n], &s) != nil || s.Fingerprint == "" {
			continue
		}
		if s.Fingerprint == r.self.Fingerprint {
			continue // ignore our own beacon
		}
		host, _, _ := net.SplitHostPort(src.String())
		r.mu.Lock()
		r.peers[s.Fingerprint] = Peer{
			Name: s.Name, Fingerprint: s.Fingerprint,
			Addr: net.JoinHostPort(host, itoa(s.HTTPPort)), HTTPPort: s.HTTPPort,
			Version: s.Version, OS: s.OS, LastSeen: time.Now(),
		}
		r.mu.Unlock()
	}
}

func (r *Registry) reap(ctx context.Context) {
	t := time.NewTicker(peerTTL)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cutoff := time.Now().Add(-peerTTL)
			r.mu.Lock()
			for fp, p := range r.peers {
				if p.LastSeen.Before(cutoff) {
					delete(r.peers, fp)
				}
			}
			r.mu.Unlock()
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [6]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
