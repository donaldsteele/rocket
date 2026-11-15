package httpapi

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/donaldsteele/rocket/internal/receiver"
)

// offerReadTimeout bounds how long an offer body (/api/offer, /drop/offer,
// at most protocol.MaxOfferBytes) may take to arrive, so a trickled body
// cannot hold a connection open for ever. A variable for tests, read when a
// Server is built.
var offerReadTimeout = receiver.DefaultOfferReadTimeout

// Resource limits for the LAN surface.
const (
	// offerBurst and offerRefill rate-limit offers per remote IP (see
	// receiver.OfferBurst).
	offerBurst  = receiver.OfferBurst
	offerRefill = receiver.OfferRefill
	// MaxConnsPerIP caps the open connections from one non-loopback address
	// (see LimitListener).
	MaxConnsPerIP = 32
)

// limitOfferRead gives an offer body offerReadTimeout to arrive (see
// receiver.LimitOfferRead).
func limitOfferRead(w http.ResponseWriter) (decoded func()) {
	return receiver.LimitOfferRead(w, offerReadTimeout)
}

// rateLimiter is a per-key token bucket (receiver.RateLimiter).
type rateLimiter = receiver.RateLimiter

func newRateLimiter(burst int, refill time.Duration) *rateLimiter {
	return receiver.NewRateLimiter(burst, refill)
}

// LimitListener wraps ln so that one remote IP holds at most perIP open
// connections at a time; loopback connections (this computer's UI and API)
// are not limited. A connection over the cap is closed at once.
func LimitListener(ln net.Listener, perIP int) net.Listener {
	return &limitListener{Listener: ln, perIP: perIP, exempt: net.IP.IsLoopback, n: map[string]int{}}
}

type limitListener struct {
	net.Listener
	perIP  int
	exempt func(net.IP) bool

	mu sync.Mutex
	n  map[string]int
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		host, _, err := net.SplitHostPort(c.RemoteAddr().String())
		ip := net.ParseIP(host)
		if err != nil || ip == nil || l.exempt(ip) {
			return c, nil
		}
		key := ip.String()
		l.mu.Lock()
		if l.n[key] >= l.perIP {
			l.mu.Unlock()
			c.Close()
			continue
		}
		l.n[key]++
		l.mu.Unlock()
		return &limitConn{Conn: c, release: func() { l.release(key) }}, nil
	}
}

func (l *limitListener) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[key]--; l.n[key] <= 0 {
		delete(l.n, key)
	}
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// CancelTransfers cancels every transfer in flight, in both directions, and
// waits until each has cleaned up (partial files removed, history written,
// the other end told) or ctx is done. Call it before shutting the HTTP server
// down, which on its own neither cancels request contexts nor waits for
// handlers past its timeout.
func (s *Server) CancelTransfers(ctx context.Context) {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for s.xfers.Len() > 0 {
		// Cancelling again is harmless and also stops a transfer that
		// started meanwhile.
		for _, t := range s.xfers.List() {
			s.xfers.Cancel(t.ID)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
