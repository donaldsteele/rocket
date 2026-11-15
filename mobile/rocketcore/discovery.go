package rocketcore

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/donaldsteele/rocket/internal/discovery"
)

// PeerListener hears about discovered peers.
type PeerListener interface {
	// OnPeers gets the full peer list ([{name, fp, addr, os, version, via,
	// proto}], sorted by name) whenever it changes. A peer whose proto is
	// not ProtoVersion runs another rocket and cannot be sent to.
	OnPeers(peersJSON string)
}

// peersPoll is how often the peer list is checked for changes.
const peersPoll = time.Second

type discovering struct {
	reg    *discovery.Registry
	cancel context.CancelFunc
	done   chan struct{}
}

// StartDiscovery listens for other hosts' beacons (hold a
// WifiManager.MulticastLock meanwhile) and reports the peer list to l on
// every change, until StopDiscovery. NsdManager results join the same list
// through ObserveMDNS. It fails if the beacon port cannot be opened.
func (n *Node) StartDiscovery(l PeerListener) error {
	if l == nil {
		return errors.New("no peer listener")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.disc != nil {
		return errors.New("discovery is already running")
	}
	reg := n.registryLocked()
	ctx, cancel := context.WithCancel(context.Background())
	if n.beaconPort > 0 {
		if err := reg.Listen(ctx); err != nil {
			cancel()
			return err
		}
	}
	d := &discovering{reg: reg, cancel: cancel, done: make(chan struct{})}
	n.disc = d
	go func() {
		defer close(d.done)
		last := ""
		t := time.NewTicker(peersPoll)
		defer t.Stop()
		for {
			if cur := peersJSON(reg); cur != last {
				last = cur
				l.OnPeers(cur)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

// StopDiscovery stops listening for beacons and reporting peers.
func (n *Node) StopDiscovery() {
	n.mu.Lock()
	d := n.disc
	n.disc = nil
	n.mu.Unlock()
	if d != nil {
		d.cancel()
		<-d.done
	}
}

// ObserveMDNS records an mDNS (NsdManager) sighting of a peer: its service
// name, the fp TXT record, the resolved host and port, and the ver, os and
// proto TXT records (a missing or unreadable proto counts as 1). A sighting
// without a valid fingerprint or port is ignored.
func (n *Node) ObserveMDNS(name, fp, host string, port int, version, osName, proto string) {
	if !discovery.IsFingerprint(fp) || host == "" || port <= 0 || port > 65535 {
		return
	}
	n.registry().Observe(discovery.ViaMDNS, discovery.Peer{
		Name: name, Fingerprint: fp, Addr: net.JoinHostPort(host, strconv.Itoa(port)),
		HTTPPort: port, Version: version, OS: osName, Proto: discovery.ParseProto(proto),
	})
}

// ForgetMDNS drops the mDNS sighting of peer fp (service lost).
func (n *Node) ForgetMDNS(fp string) { n.registry().Forget(discovery.ViaMDNS, fp) }

// PeersJSON returns the live peers, sorted by name (see peersJSON).
func (n *Node) PeersJSON() string { return peersJSON(n.registry()) }

func (n *Node) registry() *discovery.Registry {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.registryLocked()
}

// registryLocked returns the peer registry, made on first use. It is kept
// for the node's life, so sightings survive a discovery restart until they
// expire.
func (n *Node) registryLocked() *discovery.Registry {
	if n.reg == nil {
		id := n.identity()
		n.reg = discovery.New(discovery.Self{Name: id.Name, Fingerprint: id.Fingerprint(), Version: n.version, OS: OS}, n.beaconPort)
	}
	return n.reg
}

// peerView is one peer as desktop rocket's GET /api/peers shows it.
type peerView struct {
	Name    string   `json:"name"`
	FP      string   `json:"fp"`
	Addr    string   `json:"addr"`
	OS      string   `json:"os"`
	Version string   `json:"version"`
	Via     []string `json:"via"`
	Proto   int      `json:"proto"`
}

// peersJSON lists the live peers by name, then fingerprint. The registry
// lists them freshest first, which reorders hosts on every beacon (each
// announces on its own 2-second beat): on a phone the rows would keep
// swapping under a finger about to tap Files, sending to the wrong host.
func peersJSON(reg *discovery.Registry) string {
	ps := reg.Peers()
	slices.SortStableFunc(ps, func(a, b discovery.Peer) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Fingerprint, b.Fingerprint)
	})
	out := make([]peerView, 0, len(ps))
	for _, p := range ps {
		out = append(out, peerView{Name: p.Name, FP: p.Fingerprint, Addr: p.Addr, OS: p.OS, Version: p.Version, Via: p.Via, Proto: p.Proto})
	}
	return toJSON(out)
}
