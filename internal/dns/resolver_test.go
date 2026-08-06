package dns

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wadahiro/awsocks/internal/clock"
	"github.com/wadahiro/awsocks/internal/routing"
	"github.com/wadahiro/awsocks/internal/testutil/fakedns"
)

// routeDialers builds a DialFunc table where every route uses plain TCP.
func routeDialers(routes ...routing.Route) map[routing.Route]DialFunc {
	m := make(map[routing.Route]DialFunc)
	for _, r := range routes {
		m[r] = netDial
	}
	return m
}

func newTestRule(servers []string, patterns []string) Rule {
	return Rule{
		Via:         routing.RouteProxy,
		Servers:     servers,
		Patterns:    patterns,
		Timeout:     time.Second,
		MinTTL:      DefaultMinTTL,
		MaxTTL:      DefaultMaxTTL,
		NegativeTTL: DefaultNegativeTTL,
		OnFailure:   FailureFallthrough,
		Prefer:      FamilyIPv4,
	}
}

func newTestResolver(t *testing.T, clk clock.Clock, rules ...Rule) *Resolver {
	t.Helper()
	cfg := &Config{Rules: rules}
	r, err := NewResolver(cfg, routeDialers(routing.RouteProxy, routing.RouteDirect, routing.RouteVMDirect), clk)
	require.NoError(t, err)
	return r
}

func TestResolveReturnsIPForMatchingHost(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.5")},
		TTL: 60,
	})

	r := newTestResolver(t, clock.NewMockClock(time.Now()),
		newTestRule([]string{addr}, []string{"*.internal.example.com"}))

	ip, ok, err := r.Resolve(context.Background(), "app.internal.example.com")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.5", ip.String())
}

func TestResolveSkipsNonMatchingHost(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("other.example.org", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.5")},
		TTL: 60,
	})

	r := newTestResolver(t, clock.NewMockClock(time.Now()),
		newTestRule([]string{addr}, []string{"*.internal.example.com"}))

	_, ok, err := r.Resolve(context.Background(), "other.example.org")

	require.NoError(t, err)
	assert.False(t, ok, "host outside the rule patterns must not be resolved")
	assert.Equal(t, 0, srv.QueryCount(), "no query should be sent for a non-matching host")
}

func TestResolveWithEmptyPatternsMatchesEverything(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("anything.example.org", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.7")},
		TTL: 60,
	})

	r := newTestResolver(t, clock.NewMockClock(time.Now()),
		newTestRule([]string{addr}, nil))

	ip, ok, err := r.Resolve(context.Background(), "anything.example.org")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.7", ip.String())
}

func TestResolveSkipsIPLiteral(t *testing.T) {
	srv, addr := startFake(t)

	r := newTestResolver(t, clock.NewMockClock(time.Now()),
		newTestRule([]string{addr}, nil))

	_, ok, err := r.Resolve(context.Background(), "10.0.0.5")

	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, 0, srv.QueryCount())
}

func TestResolveUsesCacheOnSecondCall(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.5")},
		TTL: 60,
	})

	r := newTestResolver(t, clock.NewMockClock(time.Now()),
		newTestRule([]string{addr}, nil))

	_, _, err := r.Resolve(context.Background(), "app.internal.example.com")
	require.NoError(t, err)
	after := srv.QueryCount()

	_, ok, err := r.Resolve(context.Background(), "app.internal.example.com")
	require.NoError(t, err)
	require.True(t, ok)

	assert.Equal(t, after, srv.QueryCount(), "second resolve should be served from cache")
}

func TestResolveRequeriesAfterTTLExpiry(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.5")},
		TTL: 30,
	})

	clk := clock.NewMockClock(time.Now())
	r := newTestResolver(t, clk, newTestRule([]string{addr}, nil))

	_, _, err := r.Resolve(context.Background(), "app.internal.example.com")
	require.NoError(t, err)
	first := srv.QueryCount()

	clk.Advance(31 * time.Second)

	_, _, err = r.Resolve(context.Background(), "app.internal.example.com")
	require.NoError(t, err)

	assert.Greater(t, srv.QueryCount(), first, "expired entry should trigger a new query")
}

func TestResolveCollapsesConcurrentLookups(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.5")},
		TTL: 60,
	})
	// Hold each query open long enough for all goroutines to pile up.
	srv.Delay = 100 * time.Millisecond

	r := newTestResolver(t, clock.NewMockClock(time.Now()),
		newTestRule([]string{addr}, nil))

	const n = 50
	var wg sync.WaitGroup
	var okCount atomic.Int32
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			ip, ok, err := r.Resolve(context.Background(), "app.internal.example.com")
			if err == nil && ok && ip.String() == "10.0.0.5" {
				okCount.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(n), okCount.Load(), "all callers should get the answer")
	assert.Equal(t, 1, srv.QueryCount(), "concurrent lookups must collapse into one query")
}

func TestResolveFallthroughOnUnreachableServer(t *testing.T) {
	rule := newTestRule([]string{deadAddr(t)}, nil)
	rule.OnFailure = FailureFallthrough

	r := newTestResolver(t, clock.NewMockClock(time.Now()), rule)

	_, ok, err := r.Resolve(context.Background(), "app.internal.example.com")

	require.NoError(t, err, "fallthrough must not surface the error")
	assert.False(t, ok, "caller should fall back to the original hostname")
}

func TestResolveFailsHardWhenConfigured(t *testing.T) {
	rule := newTestRule([]string{deadAddr(t)}, nil)
	rule.OnFailure = FailureFail

	r := newTestResolver(t, clock.NewMockClock(time.Now()), rule)

	_, _, err := r.Resolve(context.Background(), "app.internal.example.com")

	require.Error(t, err)
}

func TestResolveDoesNotCacheServerFailure(t *testing.T) {
	srv, addr := startFake(t)
	srv.DropQuery = true

	r := newTestResolver(t, clock.NewMockClock(time.Now()),
		newTestRule([]string{addr}, nil))

	_, _, err := r.Resolve(context.Background(), "app.internal.example.com")
	require.NoError(t, err)
	first := srv.QueryCount()

	_, _, err = r.Resolve(context.Background(), "app.internal.example.com")
	require.NoError(t, err)

	assert.Greater(t, srv.QueryCount(), first,
		"an unreachable server is transient and must not be negatively cached")
}

func TestResolveCachesNXDOMAIN(t *testing.T) {
	srv, addr := startFake(t)

	r := newTestResolver(t, clock.NewMockClock(time.Now()),
		newTestRule([]string{addr}, nil))

	_, ok, err := r.Resolve(context.Background(), "missing.internal.example.com")
	require.NoError(t, err)
	require.False(t, ok)
	first := srv.QueryCount()

	_, ok, err = r.Resolve(context.Background(), "missing.internal.example.com")
	require.NoError(t, err)
	require.False(t, ok)

	assert.Equal(t, first, srv.QueryCount(), "NXDOMAIN should be negatively cached")
}

func TestResolveNXDOMAINFailsHardWhenConfigured(t *testing.T) {
	_, addr := startFake(t)
	rule := newTestRule([]string{addr}, nil)
	rule.OnFailure = FailureFail

	r := newTestResolver(t, clock.NewMockClock(time.Now()), rule)

	_, _, err := r.Resolve(context.Background(), "missing.internal.example.com")

	require.Error(t, err)
}

func TestResolvePrefersIPv4ByDefault(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("dual.internal.example.com", fakedns.Record{
		A:    []net.IP{net.ParseIP("10.0.0.5")},
		AAAA: []net.IP{net.ParseIP("2001:db8::1")},
		TTL:  60,
	})

	r := newTestResolver(t, clock.NewMockClock(time.Now()),
		newTestRule([]string{addr}, nil))

	ip, ok, err := r.Resolve(context.Background(), "dual.internal.example.com")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.5", ip.String())

	for _, q := range srv.Queries() {
		assert.NotEqual(t, "AAAA", q.Type.String(), "ipv4 preference must not query AAAA")
	}
}

func TestResolvePrefersIPv6WhenConfigured(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("dual.internal.example.com", fakedns.Record{
		A:    []net.IP{net.ParseIP("10.0.0.5")},
		AAAA: []net.IP{net.ParseIP("2001:db8::1")},
		TTL:  60,
	})

	rule := newTestRule([]string{addr}, nil)
	rule.Prefer = FamilyIPv6
	r := newTestResolver(t, clock.NewMockClock(time.Now()), rule)

	ip, ok, err := r.Resolve(context.Background(), "dual.internal.example.com")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "2001:db8::1", ip.String())
}

func TestResolveIPv6FallsBackToIPv4(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("v4only.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.5")},
		TTL: 60,
	})

	rule := newTestRule([]string{addr}, nil)
	rule.Prefer = FamilyIPv6
	r := newTestResolver(t, clock.NewMockClock(time.Now()), rule)

	ip, ok, err := r.Resolve(context.Background(), "v4only.internal.example.com")

	require.NoError(t, err)
	require.True(t, ok, "AAAA-less name should still resolve via A")
	assert.Equal(t, "10.0.0.5", ip.String())
}

func TestResolveUsesFirstMatchingRule(t *testing.T) {
	srvA, addrA := startFake(t)
	srvA.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.1.1.1")},
		TTL: 60,
	})
	srvB, addrB := startFake(t)
	srvB.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.2.2.2")},
		TTL: 60,
	})

	specific := newTestRule([]string{addrA}, []string{"*.internal.example.com"})
	catchAll := newTestRule([]string{addrB}, nil)

	r := newTestResolver(t, clock.NewMockClock(time.Now()), specific, catchAll)

	ip, ok, err := r.Resolve(context.Background(), "app.internal.example.com")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "10.1.1.1", ip.String(), "the earlier matching rule wins")
	assert.Equal(t, 0, srvB.QueryCount(), "later rules must not be consulted")
}

func TestResolveUsesDialerForRuleVia(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.5")},
		TTL: 60,
	})

	var proxyCalls, directCalls atomic.Int32
	dialers := map[routing.Route]DialFunc{
		routing.RouteProxy: func(ctx context.Context, network, address string) (net.Conn, error) {
			proxyCalls.Add(1)
			return netDial(ctx, network, address)
		},
		routing.RouteDirect: func(ctx context.Context, network, address string) (net.Conn, error) {
			directCalls.Add(1)
			return netDial(ctx, network, address)
		},
	}

	rule := newTestRule([]string{addr}, nil)
	rule.Via = routing.RouteDirect

	r, err := NewResolver(&Config{Rules: []Rule{rule}}, dialers, clock.NewMockClock(time.Now()))
	require.NoError(t, err)

	_, ok, err := r.Resolve(context.Background(), "app.internal.example.com")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int32(1), directCalls.Load(), "query must travel the rule's via route")
	assert.Equal(t, int32(0), proxyCalls.Load())
}

func TestResolveFailsWhenViaDialerMissing(t *testing.T) {
	_, addr := startFake(t)

	rule := newTestRule([]string{addr}, nil)
	rule.Via = routing.RouteVMDirect

	// Only a proxy dialer is available, e.g. VM mode is not enabled.
	_, err := NewResolver(&Config{Rules: []Rule{rule}},
		map[routing.Route]DialFunc{routing.RouteProxy: netDial},
		clock.NewMockClock(time.Now()))

	require.Error(t, err, "a rule referencing an unavailable route should be rejected up front")
}

func TestInvalidateForcesRequery(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.5")},
		TTL: 300,
	})

	r := newTestResolver(t, clock.NewMockClock(time.Now()),
		newTestRule([]string{addr}, nil))

	_, _, err := r.Resolve(context.Background(), "app.internal.example.com")
	require.NoError(t, err)
	first := srv.QueryCount()

	r.Invalidate("app.internal.example.com")

	_, _, err = r.Resolve(context.Background(), "app.internal.example.com")
	require.NoError(t, err)

	assert.Greater(t, srv.QueryCount(), first, "invalidated entry should be re-queried")
}

func TestResolveIsDisabledWhenNoRules(t *testing.T) {
	r, err := NewResolver(&Config{}, routeDialers(routing.RouteProxy), clock.NewMockClock(time.Now()))
	require.NoError(t, err)

	_, ok, err := r.Resolve(context.Background(), "app.internal.example.com")

	require.NoError(t, err)
	assert.False(t, ok)
}

func TestNilResolverIsInert(t *testing.T) {
	var r *Resolver

	_, ok, err := r.Resolve(context.Background(), "app.internal.example.com")

	require.NoError(t, err)
	assert.False(t, ok)

	// Must not panic.
	r.Invalidate("app.internal.example.com")
}

func TestResolveSurvivesFirstCallerCancellation(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.5")},
		TTL: 60,
	})
	srv.Delay = 200 * time.Millisecond

	rule := newTestRule([]string{addr}, nil)
	rule.Timeout = 3 * time.Second
	r := newTestResolver(t, clock.NewMockClock(time.Now()), rule)

	firstCtx, cancelFirst := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _, _ = r.Resolve(firstCtx, "app.internal.example.com")
	}()

	// Let the first caller start the shared query, then have a second caller
	// join before cancelling the first.
	time.Sleep(30 * time.Millisecond)

	var secondIP net.IP
	var secondOK bool
	var secondErr error
	go func() {
		defer wg.Done()
		secondIP, secondOK, secondErr = r.Resolve(context.Background(), "app.internal.example.com")
	}()

	time.Sleep(30 * time.Millisecond)
	cancelFirst()

	wg.Wait()

	require.NoError(t, secondErr, "second caller must not inherit the first caller's cancellation")
	require.True(t, secondOK)
	assert.Equal(t, "10.0.0.5", secondIP.String())
}

func TestNormalizeServer(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "IPv4 without port gets default port", input: "10.0.0.2", want: "10.0.0.2:53"},
		{name: "IPv4 with port is preserved", input: "10.0.0.2:5353", want: "10.0.0.2:5353"},
		{name: "IPv6 without port gets bracketed", input: "2001:db8::1", want: "[2001:db8::1]:53"},
		{name: "IPv6 with port is preserved", input: "[2001:db8::1]:5353", want: "[2001:db8::1]:5353"},
		{name: "hostname is rejected", input: "dns.example.com", wantErr: true},
		{name: "hostname with port is rejected", input: "dns.example.com:53", wantErr: true},
		{name: "empty is rejected", input: "", wantErr: true},
		{name: "invalid port is rejected", input: "10.0.0.2:99999", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeServer(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
