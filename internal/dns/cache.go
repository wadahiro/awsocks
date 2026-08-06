package dns

import (
	"net"
	"strings"
	"sync"
	"time"

	"github.com/wadahiro/awsocks/internal/clock"
)

// defaultMaxCacheEntries bounds memory use. A proxy resolves a small set of
// names, so this is a safety net rather than a tuning knob.
const defaultMaxCacheEntries = 1024

// entry is one cached resolution.
type entry struct {
	ipv4      []net.IP
	ipv6      []net.IP
	expiresAt time.Time
	negative  bool
}

// Cache stores resolution results keyed by lowercase hostname.
// Expiry is lazy: entries are dropped when Get finds them stale, avoiding a
// background sweeper that would complicate testing against a mock clock.
// It is safe for concurrent use.
type Cache struct {
	mu sync.Mutex
	m  map[string]*entry

	clock       clock.Clock
	minTTL      time.Duration
	maxTTL      time.Duration
	negativeTTL time.Duration
	maxEntries  int
}

// NewCache creates a cache clamping TTLs into [minTTL, maxTTL].
func NewCache(clk clock.Clock, minTTL, maxTTL, negativeTTL time.Duration, maxEntries int) *Cache {
	if maxEntries <= 0 {
		maxEntries = defaultMaxCacheEntries
	}
	return &Cache{
		m:           make(map[string]*entry),
		clock:       clk,
		minTTL:      minTTL,
		maxTTL:      maxTTL,
		negativeTTL: negativeTTL,
		maxEntries:  maxEntries,
	}
}

// Get returns the entry for host if present and unexpired.
func (c *Cache) Get(host string) (*entry, bool) {
	key := strings.ToLower(host)

	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if !c.clock.Now().Before(e.expiresAt) {
		delete(c.m, key)
		return nil, false
	}
	return e, true
}

// Put stores a positive result, clamping ttl into [minTTL, maxTTL].
func (c *Cache) Put(host string, ipv4, ipv6 []net.IP, ttl time.Duration) {
	c.store(strings.ToLower(host), &entry{
		ipv4:      ipv4,
		ipv6:      ipv6,
		expiresAt: c.clock.Now().Add(c.clampTTL(ttl)),
	})
}

// PutNegative records that host did not resolve, for negativeTTL.
func (c *Cache) PutNegative(host string) {
	c.store(strings.ToLower(host), &entry{
		negative:  true,
		expiresAt: c.clock.Now().Add(c.negativeTTL),
	})
}

// Delete removes the entry for host.
func (c *Cache) Delete(host string) {
	key := strings.ToLower(host)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

// Len returns the number of entries currently held, expired or not.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

func (c *Cache) store(key string, e *entry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.m[key]; !exists && len(c.m) >= c.maxEntries {
		c.evictLocked()
	}
	c.m[key] = e
}

// evictLocked drops the entry expiring soonest. Callers must hold c.mu.
func (c *Cache) evictLocked() {
	now := c.clock.Now()

	// Prefer reclaiming already-expired entries, which costs nothing.
	evicted := false
	for k, e := range c.m {
		if !now.Before(e.expiresAt) {
			delete(c.m, k)
			evicted = true
		}
	}
	if evicted {
		return
	}

	var oldestKey string
	var oldestAt time.Time
	for k, e := range c.m {
		if oldestKey == "" || e.expiresAt.Before(oldestAt) {
			oldestKey, oldestAt = k, e.expiresAt
		}
	}
	if oldestKey != "" {
		delete(c.m, oldestKey)
	}
}

func (c *Cache) clampTTL(ttl time.Duration) time.Duration {
	if ttl < c.minTTL {
		return c.minTTL
	}
	if c.maxTTL > 0 && ttl > c.maxTTL {
		return c.maxTTL
	}
	return ttl
}
