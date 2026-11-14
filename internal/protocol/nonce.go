package protocol

import (
	"container/list"
	"sync"
	"time"
)

// NonceCache remembers recently used offer nonces so a captured offer cannot
// be replayed. It is a bounded LRU; entries older than twice the clock-skew
// window are also forgotten, since the timestamp check rejects those offers
// anyway. Safe for concurrent use.
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

// Use records key as used at now. It returns false if key was already used
// (a replay), true if this is its first use.
func (c *NonceCache) Use(key string, now time.Time) bool {
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
		return false
	}
	c.index[key] = c.ll.PushFront(&nonceEntry{key: key, seen: now})
	for c.ll.Len() > c.cap {
		e := c.ll.Back()
		c.ll.Remove(e)
		delete(c.index, e.Value.(*nonceEntry).key)
	}
	return true
}

// Len reports how many nonces are remembered.
func (c *NonceCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
