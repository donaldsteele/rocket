package protocol

import (
	"container/list"
	"sync"
	"time"
)

// NonceCache remembers recently used offer nonces so a captured offer cannot
// be replayed. Entries are forgotten only once they are older than twice the
// clock-skew window, when the timestamp check rejects their offers anyway. A
// nonce that could still pass that check is never evicted to make room: when
// the cache is full of such nonces, new offers are refused (ErrBusy) until
// the oldest ages out, so flooding the cache can delay offers but never make
// a replay possible. Safe for concurrent use.
type NonceCache struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List // front = newest
	index map[string]*list.Element
}

type nonceEntry struct {
	key  string
	seen time.Time
}

// NewNonceCache returns a cache holding up to capacity nonces.
func NewNonceCache(capacity int) *NonceCache {
	if capacity <= 0 {
		capacity = NonceCacheSize
	}
	return &NonceCache{cap: capacity, ll: list.New(), index: map[string]*list.Element{}}
}

// Use records key as used at now. It returns nil on the key's first use,
// ErrReplay if it was already used, and ErrBusy if the cache is full of
// nonces that are still inside the freshness window (key is not recorded).
func (c *NonceCache) Use(key string, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Forget entries that can no longer pass the freshness check.
	for e := c.ll.Back(); e != nil; e = c.ll.Back() {
		if now.Sub(e.Value.(*nonceEntry).seen) <= 2*MaxClockSkew {
			break
		}
		c.ll.Remove(e)
		delete(c.index, e.Value.(*nonceEntry).key)
	}
	if _, ok := c.index[key]; ok {
		return ErrReplay
	}
	if c.ll.Len() >= c.cap {
		return ErrBusy
	}
	c.index[key] = c.ll.PushFront(&nonceEntry{key: key, seen: now})
	return nil
}

// Len reports how many nonces are remembered.
func (c *NonceCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
