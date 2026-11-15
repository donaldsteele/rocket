package receiver

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/donaldsteele/rocket/internal/protocol"
)

// Limits for the peer-facing surface.
const (
	// OfferBurst and OfferRefill rate-limit offers per remote IP: a burst of
	// OfferBurst, then one more every OfferRefill. A real sender makes one
	// offer per send; the limit keeps one host from spending the receiver's
	// replay cache or flooding it with prompts.
	OfferBurst  = 30
	OfferRefill = 2 * time.Second
	// DefaultOfferReadTimeout bounds how long an offer body (at most
	// protocol.MaxOfferBytes) may take to arrive, so a trickled body cannot
	// hold a connection open for ever.
	DefaultOfferReadTimeout = 30 * time.Second
)

// LimitOfferRead gives an offer body timeout to arrive. Call the returned
// func once the body decoded, before waiting for the person's decision: it
// moves the deadline past protocol.OfferWait, because an expired read
// deadline would end the request. The deadline is never lifted, so the
// server's drain of any unread body after the handler is bounded too. (The
// server sets a fresh deadline for the next request on the connection.)
func LimitOfferRead(w http.ResponseWriter, timeout time.Duration) (decoded func()) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(timeout))
	return func() { _ = rc.SetReadDeadline(time.Now().Add(protocol.OfferWait + timeout)) }
}

// RateLimiter is a per-key token bucket, safe for concurrent use.
type RateLimiter struct {
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

// NewRateLimiter allows a burst of burst calls per key, then one more every
// refill.
func NewRateLimiter(burst int, refill time.Duration) *RateLimiter {
	return &RateLimiter{burst: float64(burst), refill: refill, now: time.Now, m: map[string]*bucket{}}
}

// Allow takes one token from key's bucket and reports whether there was one.
func (l *RateLimiter) Allow(key string) bool {
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

func (l *RateLimiter) fill(b *bucket, now time.Time) float64 {
	return min(l.burst, b.tokens+float64(now.Sub(b.last))/float64(l.refill))
}

// RemoteIP is the peer IP of r, used for per-IP limits and token binding.
func RemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
