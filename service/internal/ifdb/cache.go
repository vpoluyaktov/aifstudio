package ifdb

import (
	"sync"
	"time"
)

// entry is an in-memory cache entry.
type entry struct {
	data      []byte // JSON-encoded Game
	expiresAt time.Time
}

// memCache is an in-memory TTL cache keyed by IFDB TUID.
// Freshness check uses strict > (an entry equal to now is considered expired).
type memCache struct {
	mu      sync.Mutex
	entries map[string]*entry
}

func newMemCache() *memCache {
	return &memCache{entries: make(map[string]*entry)}
}

// get returns the cached bytes and true if the entry exists and expiresAt > now.
func (c *memCache) get(tuid string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[tuid]
	if !ok {
		return nil, false
	}
	if !e.expiresAt.After(time.Now()) {
		delete(c.entries, tuid)
		return nil, false
	}
	return e.data, true
}

// set stores data with the given expiry.
func (c *memCache) set(tuid string, data []byte, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[tuid] = &entry{data: data, expiresAt: expiresAt}
}

// seed pre-populates the cache with entries from the Firestore cold-start warm-up.
func (c *memCache) seed(tuid string, data []byte, expiresAt time.Time) {
	if !expiresAt.After(time.Now()) {
		return
	}
	c.set(tuid, data, expiresAt)
}
