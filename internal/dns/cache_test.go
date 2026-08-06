package dns

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wadahiro/awsocks/internal/clock"
)

func newTestCache(clk clock.Clock) *Cache {
	return NewCache(clk, 10*time.Second, 5*time.Minute, 5*time.Second, defaultMaxCacheEntries)
}

func TestCacheReturnsStoredEntry(t *testing.T) {
	clk := clock.NewMockClock(time.Now())
	c := newTestCache(clk)

	c.Put("app.internal", []net.IP{net.ParseIP("10.0.0.5")}, nil, time.Minute)

	e, ok := c.Get("app.internal")
	require.True(t, ok)
	require.Len(t, e.ipv4, 1)
	assert.Equal(t, "10.0.0.5", e.ipv4[0].String())
	assert.False(t, e.negative)
}

func TestCacheExpiresAfterTTL(t *testing.T) {
	clk := clock.NewMockClock(time.Now())
	c := newTestCache(clk)

	c.Put("app.internal", []net.IP{net.ParseIP("10.0.0.5")}, nil, time.Minute)

	clk.Advance(59 * time.Second)
	_, ok := c.Get("app.internal")
	assert.True(t, ok, "should still be cached just before expiry")

	clk.Advance(2 * time.Second)
	_, ok = c.Get("app.internal")
	assert.False(t, ok, "should be expired")
}

func TestCacheClampsTTLToMinimum(t *testing.T) {
	clk := clock.NewMockClock(time.Now())
	c := newTestCache(clk)

	// A 1s TTL would otherwise cause a tunnel round-trip on nearly every dial.
	c.Put("app.internal", []net.IP{net.ParseIP("10.0.0.5")}, nil, time.Second)

	clk.Advance(5 * time.Second)
	_, ok := c.Get("app.internal")
	assert.True(t, ok, "TTL below min-ttl should be raised to min-ttl")

	clk.Advance(6 * time.Second)
	_, ok = c.Get("app.internal")
	assert.False(t, ok)
}

func TestCacheClampsTTLToMaximum(t *testing.T) {
	clk := clock.NewMockClock(time.Now())
	c := newTestCache(clk)

	c.Put("app.internal", []net.IP{net.ParseIP("10.0.0.5")}, nil, 24*time.Hour)

	clk.Advance(5*time.Minute + time.Second)
	_, ok := c.Get("app.internal")
	assert.False(t, ok, "TTL above max-ttl should be capped at max-ttl")
}

func TestCacheNegativeEntryExpiresAfterNegativeTTL(t *testing.T) {
	clk := clock.NewMockClock(time.Now())
	c := newTestCache(clk)

	c.PutNegative("missing.internal")

	e, ok := c.Get("missing.internal")
	require.True(t, ok)
	assert.True(t, e.negative)

	clk.Advance(6 * time.Second)
	_, ok = c.Get("missing.internal")
	assert.False(t, ok)
}

func TestCacheDeleteRemovesEntry(t *testing.T) {
	clk := clock.NewMockClock(time.Now())
	c := newTestCache(clk)

	c.Put("app.internal", []net.IP{net.ParseIP("10.0.0.5")}, nil, time.Minute)
	c.Delete("app.internal")

	_, ok := c.Get("app.internal")
	assert.False(t, ok)
}

func TestCacheIsCaseInsensitive(t *testing.T) {
	clk := clock.NewMockClock(time.Now())
	c := newTestCache(clk)

	c.Put("App.Internal", []net.IP{net.ParseIP("10.0.0.5")}, nil, time.Minute)

	_, ok := c.Get("app.INTERNAL")
	assert.True(t, ok)

	c.Delete("APP.internal")
	_, ok = c.Get("app.internal")
	assert.False(t, ok)
}

func TestCacheEvictsWhenOverCapacity(t *testing.T) {
	clk := clock.NewMockClock(time.Now())
	c := NewCache(clk, 10*time.Second, 5*time.Minute, 5*time.Second, 3)

	// Longer TTL survives; shorter ones are evicted first.
	c.Put("keep.internal", []net.IP{net.ParseIP("10.0.0.1")}, nil, 5*time.Minute)
	c.Put("drop1.internal", []net.IP{net.ParseIP("10.0.0.2")}, nil, 11*time.Second)
	c.Put("drop2.internal", []net.IP{net.ParseIP("10.0.0.3")}, nil, 12*time.Second)
	c.Put("new.internal", []net.IP{net.ParseIP("10.0.0.4")}, nil, 5*time.Minute)

	assert.LessOrEqual(t, c.Len(), 3)

	_, ok := c.Get("keep.internal")
	assert.True(t, ok, "entry with the longest TTL should survive eviction")
	_, ok = c.Get("new.internal")
	assert.True(t, ok, "newly inserted entry should be present")
	_, ok = c.Get("drop1.internal")
	assert.False(t, ok, "soonest-expiring entry should be evicted")
}

func TestCacheStoresBothFamilies(t *testing.T) {
	clk := clock.NewMockClock(time.Now())
	c := newTestCache(clk)

	c.Put("dual.internal",
		[]net.IP{net.ParseIP("10.0.0.5")},
		[]net.IP{net.ParseIP("2001:db8::1")},
		time.Minute)

	e, ok := c.Get("dual.internal")
	require.True(t, ok)
	require.Len(t, e.ipv4, 1)
	require.Len(t, e.ipv6, 1)
	assert.Equal(t, "10.0.0.5", e.ipv4[0].String())
	assert.Equal(t, "2001:db8::1", e.ipv6[0].String())
}
