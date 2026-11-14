package discovery

// mDNS (DNS-SD): advertise this host as <name>._rocket._tcp.local on the HTTP
// port, and browse the same type. Results feed the registry tagged "mdns".

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/brutella/dnssd"
	dnslog "github.com/brutella/dnssd/log"
)

// ServiceType is the DNS-SD service type rocket advertises and browses.
const ServiceType = "_rocket._tcp"

// TXTProto is the value of the "proto" TXT key: the wire protocol version.
const TXTProto = "1"

// browseInterval is how long one browse runs before it is restarted with a
// fresh query, so peers that still answer are seen again.
const browseInterval = 30 * time.Second

func init() {
	// dnssd writes its INFO lines (other devices' protocol slips) to stdout,
	// which carries MCP frames in `rocket mcp`. Silence them.
	dnslog.Info.Disable()
}

// TXT builds the TXT record map: fp, ver, os and proto=1.
func TXT(self Self) map[string]string {
	return map[string]string{
		"fp":    self.Fingerprint,
		"ver":   self.Version,
		"os":    self.OS,
		"proto": TXTProto,
	}
}

// AdvertiseOptions configures an Advertiser.
type AdvertiseOptions struct {
	// Instance is the service instance name (the device name).
	Instance string
	// Host is the single-label mDNS host name; <Host>.local is published.
	// Empty means the OS host name.
	Host string
	// Port is the HTTP port the service lives on.
	Port int
	// Text is the TXT record.
	Text map[string]string
	// Ifaces are the interface names to publish on; empty lets dnssd use every
	// multicast interface.
	Ifaces []string
	// Logger receives diagnostics; nil discards.
	Logger *slog.Logger
}

// Advertiser owns one mDNS responder.
type Advertiser struct {
	opts AdvertiseOptions
	log  *slog.Logger

	mu     sync.Mutex
	resp   dnssd.Responder
	cancel context.CancelFunc
	done   chan struct{}
	host   string
	inst   string
}

// NewAdvertiser returns an idle Advertiser.
func NewAdvertiser(opts AdvertiseOptions) *Advertiser {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Advertiser{opts: opts, log: log}
}

// Advertiser returns an Advertiser for this host: instance = device name,
// host = rocket-<first 8 fp digits> (so it never collides with the OS's own
// <hostname>.local), port = HTTP port, TXT from TXT(self).
func (r *Registry) Advertiser(log *slog.Logger) *Advertiser {
	host := "rocket"
	if fp := r.self.Fingerprint; len(fp) >= 8 {
		host += "-" + fp[:8]
	}
	return NewAdvertiser(AdvertiseOptions{
		Instance: r.self.Name,
		Host:     host,
		Port:     r.self.HTTPPort,
		Text:     TXT(r.self),
		Logger:   log,
	})
}

// Start probes for unique names, then announces the service until ctx is
// cancelled or Close is called. It blocks for the probe only; responding
// continues in the background. A name conflict renames the instance or host
// (dnssd appends a number) and is logged.
func (a *Advertiser) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.resp != nil {
		return errors.New("mdns: already started")
	}
	if a.opts.Port <= 0 {
		return errors.New("mdns: no port")
	}
	svc, err := dnssd.NewService(dnssd.Config{
		Name:   a.opts.Instance,
		Type:   ServiceType,
		Host:   a.opts.Host,
		Text:   a.opts.Text,
		Port:   a.opts.Port,
		Ifaces: a.opts.Ifaces,
	})
	if err != nil {
		return fmt.Errorf("mdns: build service: %w", err)
	}
	probed, err := dnssd.ProbeService(ctx, svc)
	if err != nil {
		return fmt.Errorf("mdns: probe names: %w", err)
	}
	renamed := probed.Host != svc.Host || probed.Name != svc.Name
	resp, err := dnssd.NewResponder()
	if err != nil {
		return fmt.Errorf("mdns: new responder: %w", err)
	}
	if _, err := resp.Add(probed); err != nil {
		return fmt.Errorf("mdns: add service: %w", err)
	}
	rctx, cancel := context.WithCancel(ctx)
	a.resp, a.cancel, a.done = resp, cancel, make(chan struct{})
	a.host, a.inst = probed.Host, probed.Name
	go func(done chan struct{}) {
		defer close(done)
		if err := resp.Respond(rctx); err != nil && !errors.Is(err, context.Canceled) {
			a.log.Warn("mdns responder stopped", "error", err.Error())
		}
	}(a.done)
	if renamed {
		a.log.Info("mdns name conflict, renamed", "host", probed.Hostname(), "instance", probed.Name)
	}
	return nil
}

// Names returns the host and instance names actually in use after probing.
func (a *Advertiser) Names() (host, instance string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.host, a.inst
}

// Close stops responding and waits for the responder to exit.
func (a *Advertiser) Close() {
	a.mu.Lock()
	cancel, done := a.cancel, a.done
	a.resp, a.cancel, a.done = nil, nil, nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Browse looks for other rocket hosts over mDNS until ctx is cancelled,
// feeding the registry. Each browse runs for browseInterval and is then
// restarted. It returns an error only when the first browse cannot start (no
// multicast socket, for example); later failures are logged and retried.
func (r *Registry) Browse(ctx context.Context, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	first := true
	for {
		bctx, cancel := context.WithTimeout(ctx, browseInterval)
		err := dnssd.LookupType(bctx, ServiceType+".local.", r.mdnsAdd, r.mdnsRemove)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			if first {
				return fmt.Errorf("mdns: browse: %w", err)
			}
			log.Warn("mdns browse failed; retrying", "error", err.Error())
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(browseInterval):
			}
		}
		first = false
	}
}

func (r *Registry) mdnsAdd(e dnssd.BrowseEntry) {
	if p, ok := entryPeer(e); ok {
		r.Observe(ViaMDNS, p)
	}
}

func (r *Registry) mdnsRemove(e dnssd.BrowseEntry) {
	if fp := e.Text["fp"]; isFingerprint(fp) {
		r.forget(ViaMDNS, fp)
	}
}

// entryPeer converts a browse result to a Peer. It rejects entries without a
// valid fingerprint, with another protocol version, a bad port or no usable
// address.
func entryPeer(e dnssd.BrowseEntry) (Peer, bool) {
	fp := e.Text["fp"]
	if !isFingerprint(fp) || e.Text["proto"] != TXTProto {
		return Peer{}, false
	}
	if e.Port <= 0 || e.Port > 65535 {
		return Peer{}, false
	}
	ip := pickIP(e.IPs)
	if ip == nil {
		return Peer{}, false
	}
	return Peer{
		Name: e.Name, Fingerprint: fp,
		Addr:     net.JoinHostPort(ip.String(), strconv.Itoa(e.Port)),
		HTTPPort: e.Port, Version: e.Text["ver"], OS: e.Text["os"],
	}, true
}

// pickIP prefers the first IPv4 address (a 169.254.x.x one only when there is
// no other), then the first routable IPv6 one. Link-local IPv6 addresses are
// skipped: they need a zone, which does not fit in a URL host without
// escaping.
func pickIP(ips []net.IP) net.IP {
	var linkLocal net.IP
	for _, ip := range ips {
		v4 := ip.To4()
		if v4 == nil || v4.IsUnspecified() || v4.IsMulticast() {
			continue
		}
		if !v4.IsLinkLocalUnicast() {
			return v4
		}
		if linkLocal == nil {
			linkLocal = v4
		}
	}
	if linkLocal != nil {
		return linkLocal
	}
	for _, ip := range ips {
		if len(ip) == net.IPv6len && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast() && !ip.IsMulticast() {
			return ip
		}
	}
	return nil
}

// isFingerprint reports whether s looks like a node fingerprint: 16 lowercase
// hex digits.
func isFingerprint(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
