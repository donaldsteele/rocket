package httpapi

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/donaldsteele/rocket/internal/protocol"
)

// offerReadTimeout bounds how long an offer body (/api/offer, /drop/offer,
// at most protocol.MaxOfferBytes) may take to arrive, so a trickled body
// cannot hold a connection open for ever. A variable for tests.
var offerReadTimeout = 30 * time.Second

// Resource limits for the LAN surface.
const (
	// offerBurst and offerRefill rate-limit offers per remote IP: a burst of
	// offerBurst, then one more every offerRefill. A real sender makes one
	// offer per send; the limit keeps one host from spending the receiver's
	// replay cache or flooding it with prompts.
	offerBurst  = 30
	offerRefill = 2 * time.Second
	// MaxConnsPerIP caps the open connections from one non-loopback address
	// (see LimitListener).
	MaxConnsPerIP = 32
)

// limitOfferRead gives an offer body offerReadTimeout to arrive. Call the
// returned func once the body decoded, before waiting for the person's
// decision: it moves the deadline past protocol.OfferWait, because an expired
// read deadline would end the request. The deadline is never lifted, so the
// server's drain of any unread body after the handler is bounded too. (The
// server sets a fresh deadline for the next request on the connection.)
func limitOfferRead(w http.ResponseWriter) (decoded func()) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(offerReadTimeout))
	return func() { _ = rc.SetReadDeadline(time.Now().Add(protocol.OfferWait + offerReadTimeout)) }
}

// rateLimiter is a per-key token bucket.
type rateLimiter struct {
	burst  float64
	refill time.Duration
	now    func() time.Time

	mu sync.Mutex
	m  map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(burst int, refill time.Duration) *rateLimiter {
	return &rateLimiter{burst: float64(burst), refill: refill, now: time.Now, m: map[string]*bucket{}}
}

// allow takes one token from key's bucket and reports whether there was one.
func (l *rateLimiter) allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) >= 4096 {
		// Forget buckets that have refilled completely: they behave exactly
		// like a new one.
		for k, b := range l.m {
			if l.fill(b, now) >= l.burst {
				delete(l.m, k)
			}
		}
	}
	b := l.m[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.m[key] = b
	}
	b.tokens, b.last = l.fill(b, now), now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *rateLimiter) fill(b *bucket, now time.Time) float64 {
	return min(l.burst, b.tokens+float64(now.Sub(b.last))/float64(l.refill))
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
