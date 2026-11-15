package receiver

import (
	"testing"
	"time"
)

// Moved from internal/httpapi with the rate limiter.
func TestRateLimiterRefills(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewRateLimiter(2, time.Second)
	l.now = func() time.Time { return now }
	if !l.Allow("a") || !l.Allow("a") || l.Allow("a") {
		t.Fatal("burst of 2 not enforced")
	}
	if !l.Allow("b") {
		t.Fatal("another IP was limited")
	}
	now = now.Add(time.Second)
	if !l.Allow("a") || l.Allow("a") {
		t.Fatal("one token per refill interval expected")
	}
}
