package proxy

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wadahiro/awsocks/internal/clock"
	"github.com/wadahiro/awsocks/internal/dns"
	"github.com/wadahiro/awsocks/internal/routing"
	"github.com/wadahiro/awsocks/internal/testutil/fakedns"
)

// recordingDialer records the addresses it was asked to dial.
type recordingDialer struct {
	mu    sync.Mutex
	addrs []string
	err   error
}

func (r *recordingDialer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	r.mu.Lock()
	r.addrs = append(r.addrs, address)
	r.mu.Unlock()

	if r.err != nil {
		return nil, r.err
	}
	client, server := net.Pipe()
	go func() { _ = server.Close() }()
	return client, nil
}

func (r *recordingDialer) dialed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.addrs))
	copy(out, r.addrs)
	return out
}

func startFakeDNS(t *testing.T, name string, ip string) string {
	t.Helper()
	srv := fakedns.NewServer()
	addr, err := srv.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetRecord(name, fakedns.Record{A: []net.IP{net.ParseIP(ip)}, TTL: 60})
	return addr
}

// newDNSResolver builds a resolver whose queries all go over plain TCP,
// standing in for whichever route the rule names.
func newDNSResolver(t *testing.T, rules []dns.RuleSpec) *dns.Resolver {
	t.Helper()
	cfg, err := dns.BuildConfig(rules)
	require.NoError(t, err)

	tcpDial := func(ctx context.Context, network, address string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, address)
	}
	dialers := map[routing.Route]dns.DialFunc{
		routing.RouteProxy:    tcpDial,
		routing.RouteDirect:   tcpDial,
		routing.RouteVMDirect: tcpDial,
	}

	r, err := dns.NewResolver(cfg, dialers, clock.NewMockClock(time.Now()))
	require.NoError(t, err)
	return r
}

func TestDialResolvesProxyRouteHostToIP(t *testing.T) {
	dnsAddr := startFakeDNS(t, "app.internal.example.com", "10.0.0.5")

	router := routing.NewRouter(&routing.Config{Default: "proxy"})
	d := NewProxyDialer(router, nil)

	backend := &recordingDialer{}
	d.SetBackendDialer(backend)
	d.SetResolver(newDNSResolver(t, []dns.RuleSpec{{Servers: []string{dnsAddr}}}))

	conn, err := d.Dial(context.Background(), "tcp", "app.internal.example.com:8080")
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, []string{"10.0.0.5:8080"}, backend.dialed(),
		"proxy route should dial the resolved IP with the port preserved")
}

func TestDialLeavesDirectRouteToOSResolverWhenNoRuleMatches(t *testing.T) {
	dnsAddr := startFakeDNS(t, "app.internal.example.com", "10.0.0.5")

	// The rule only covers *.internal.example.com; the direct host is elsewhere.
	router := routing.NewRouter(&routing.Config{
		Default: "proxy",
		Direct:  []string{"*.example.org"},
	})
	d := NewProxyDialer(router, nil)
	d.SetResolver(newDNSResolver(t, []dns.RuleSpec{{
		Servers:  []string{dnsAddr},
		Patterns: []string{"*.internal.example.com"},
	}}))

	// Dialing a direct host that no rule covers must not be rewritten. It will
	// fail to connect, but the point is that the address stays a hostname.
	_, err := d.Dial(context.Background(), "tcp", "nonexistent.example.org:80")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "10.0.0.5")
}

func TestDialResolvesDirectRouteWhenRuleMatches(t *testing.T) {
	dnsAddr := startFakeDNS(t, "app.example.org", "127.0.0.1")

	// A listener stands in for the resolved target so the dial can succeed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	router := routing.NewRouter(&routing.Config{
		Default: "proxy",
		Direct:  []string{"*.example.org"},
	})
	d := NewProxyDialer(router, nil)
	d.SetResolver(newDNSResolver(t, []dns.RuleSpec{{
		Via:      "direct",
		Servers:  []string{dnsAddr},
		Patterns: []string{"*.example.org"},
	}}))

	// The name resolves to 127.0.0.1, so the direct dial reaches the listener.
	conn, err := d.Dial(context.Background(), "tcp", "app.example.org:"+port)
	require.NoError(t, err, "direct route should use the DNS-resolved address")
	defer conn.Close()
}

func TestDialRoutingDecisionUsesOriginalHostname(t *testing.T) {
	dnsAddr := startFakeDNS(t, "app.internal.example.com", "10.0.0.5")

	// The host is routed proxy by name. If routing ran after resolution, the
	// IP would not match this pattern and the route would change.
	router := routing.NewRouter(&routing.Config{
		Default: "direct",
		Proxy:   []string{"*.internal.example.com"},
	})
	d := NewProxyDialer(router, nil)

	backend := &recordingDialer{}
	d.SetBackendDialer(backend)
	d.SetResolver(newDNSResolver(t, []dns.RuleSpec{{Servers: []string{dnsAddr}}}))

	conn, err := d.Dial(context.Background(), "tcp", "app.internal.example.com:443")
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, []string{"10.0.0.5:443"}, backend.dialed(),
		"the proxy route chosen by hostname must still be used after resolution")
}

func TestDialHostsMappingWinsOverDNS(t *testing.T) {
	dnsAddr := startFakeDNS(t, "app.internal.example.com", "10.0.0.5")

	router := routing.NewRouter(&routing.Config{
		Default: "proxy",
		Hosts:   map[string]string{"app.internal.example.com": "10.9.9.9"},
	})
	d := NewProxyDialer(router, nil)

	backend := &recordingDialer{}
	d.SetBackendDialer(backend)
	d.SetResolver(newDNSResolver(t, []dns.RuleSpec{{Servers: []string{dnsAddr}}}))

	conn, err := d.Dial(context.Background(), "tcp", "app.internal.example.com:443")
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, []string{"10.9.9.9:443"}, backend.dialed(),
		"an explicit hosts entry must take precedence over DNS")
}

func TestDialFallbackUsesOriginalHostnameNotResolvedIP(t *testing.T) {
	dnsAddr := startFakeDNS(t, "app.internal.example.com", "10.0.0.5")

	router := routing.NewRouter(&routing.Config{Default: "proxy"})
	d := NewProxyDialer(router, nil)

	// The proxy dial fails with an error that triggers the direct fallback.
	backend := &recordingDialer{err: errors.New("dial tcp: connect: no route to host")}
	d.SetBackendDialer(backend)
	d.SetResolver(newDNSResolver(t, []dns.RuleSpec{{Servers: []string{dnsAddr}}}))

	// The fallback dials directly and fails, but what matters is the address
	// it used: a VPC-internal IP reached from the host's own network could
	// land on an unrelated machine.
	_, err := d.Dial(context.Background(), "tcp", "app.internal.example.com:8080")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "10.0.0.5",
		"the fallback route must dial the hostname, never the resolved IP")
}

func TestDialInvalidatesCacheOnFallbackableError(t *testing.T) {
	srv := fakedns.NewServer()
	dnsAddr, err := srv.Start()
	require.NoError(t, err)
	defer srv.Close()
	srv.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.5")},
		TTL: 300,
	})

	router := routing.NewRouter(&routing.Config{Default: "proxy"})
	d := NewProxyDialer(router, nil)

	backend := &recordingDialer{err: errors.New("dial tcp: connect: no route to host")}
	d.SetBackendDialer(backend)
	d.SetResolver(newDNSResolver(t, []dns.RuleSpec{{Servers: []string{dnsAddr}}}))

	_, _ = d.Dial(context.Background(), "tcp", "app.internal.example.com:8080")
	first := srv.QueryCount()

	_, _ = d.Dial(context.Background(), "tcp", "app.internal.example.com:8080")

	assert.Greater(t, srv.QueryCount(), first,
		"an unreachable resolved address should be re-queried rather than served from cache")
}

func TestDialWithoutResolverIsUnchanged(t *testing.T) {
	router := routing.NewRouter(&routing.Config{Default: "proxy"})
	d := NewProxyDialer(router, nil)

	backend := &recordingDialer{}
	d.SetBackendDialer(backend)
	// No resolver configured.

	conn, err := d.Dial(context.Background(), "tcp", "app.internal.example.com:8080")
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, []string{"app.internal.example.com:8080"}, backend.dialed(),
		"with no resolver the hostname must pass through untouched")
}

func TestDialSkipsResolutionForIPLiteral(t *testing.T) {
	dnsAddr := startFakeDNS(t, "app.internal.example.com", "10.0.0.5")

	router := routing.NewRouter(&routing.Config{Default: "proxy"})
	d := NewProxyDialer(router, nil)

	backend := &recordingDialer{}
	d.SetBackendDialer(backend)
	d.SetResolver(newDNSResolver(t, []dns.RuleSpec{{Servers: []string{dnsAddr}}}))

	conn, err := d.Dial(context.Background(), "tcp", "203.0.113.7:443")
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, []string{"203.0.113.7:443"}, backend.dialed())
}

func TestDialSkipsResolutionForUpstreamProxyHost(t *testing.T) {
	dnsAddr := startFakeDNS(t, "app.partner.example.com", "10.0.0.5")

	router := routing.NewRouter(&routing.Config{Default: "proxy"})
	d := NewProxyDialer(router, nil)

	backend := &upstreamProxyDialer{
		recordingDialer: recordingDialer{},
		patterns:        []string{"*.partner.example.com"},
	}
	d.SetBackendDialer(backend)
	d.SetResolver(newDNSResolver(t, []dns.RuleSpec{{Servers: []string{dnsAddr}}}))

	conn, err := d.Dial(context.Background(), "tcp", "app.partner.example.com:443")
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, []string{"app.partner.example.com:443"}, backend.dialed(),
		"upstream proxy targets keep their hostname so the proxy resolves them")
}

// upstreamProxyDialer reports some addresses as upstream-proxy targets.
type upstreamProxyDialer struct {
	recordingDialer
	patterns []string
}

func (u *upstreamProxyDialer) UsesUpstreamProxy(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	for _, p := range u.patterns {
		if routing.ParseMatcher(p).Match(host) {
			return true
		}
	}
	return false
}

func TestDialPreConnectRouteSkipsResolution(t *testing.T) {
	dnsAddr := startFakeDNS(t, "auth.example.com", "10.0.0.5")

	router := routing.NewRouter(&routing.Config{
		Default:          "proxy",
		PreConnectDirect: []string{"auth.example.com"},
	})
	d := NewProxyDialer(router, nil)
	d.SetResolver(newDNSResolver(t, []dns.RuleSpec{{Servers: []string{dnsAddr}}}))

	// Backend is not initialized, so the pre-connect override applies.
	init := &stubLazyInitializer{done: make(chan struct{})}
	d.SetLazyInitializer(init)

	_, err := d.Dial(context.Background(), "tcp", "auth.example.com:443")

	// The dial fails because auth.example.com does not exist, but it must have
	// been attempted by name: the pre-connect path deliberately bypasses the
	// backend, so a tunnel-resolved address does not apply to it.
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "10.0.0.5")
}

// stubLazyInitializer never completes initialization.
type stubLazyInitializer struct {
	done chan struct{}
}

func (s *stubLazyInitializer) EnsureInitialized(ctx context.Context) error { return nil }
func (s *stubLazyInitializer) InitDone() <-chan struct{}                   { return s.done }
func (s *stubLazyInitializer) InitError() error                            { return nil }

// notReadyDialer wraps recordingDialer and reports itself as not Ready,
// simulating a backend that initialized once but is now mid-reconnect after
// a dropped tunnel.
type notReadyDialer struct {
	recordingDialer
}

func (n *notReadyDialer) Ready() bool { return false }

func TestDialPreConnectRouteAppliesDuringReconnect(t *testing.T) {
	router := routing.NewRouter(&routing.Config{
		Default:          "proxy",
		PreConnectDirect: []string{"auth.example.com"},
	})
	d := NewProxyDialer(router, nil)

	// Initialization already completed once...
	init := &stubLazyInitializer{done: make(chan struct{})}
	close(init.done)
	d.SetLazyInitializer(init)

	// ...but the backend has since dropped its tunnel and is reconnecting.
	backend := &notReadyDialer{}
	d.SetBackendDialer(backend)

	_, err := d.Dial(context.Background(), "tcp", "auth.example.com:443")

	// The pre-connect override dials directly by name (no DNS mock here, so
	// this fails), never through the backend.
	require.Error(t, err)
	assert.Empty(t, backend.dialed())
}

func TestDialWaitsWhenBackendNotReadyWithoutPreConnect(t *testing.T) {
	router := routing.NewRouter(&routing.Config{Default: "proxy"})
	d := NewProxyDialer(router, nil)

	init := &stubLazyInitializer{done: make(chan struct{})}
	close(init.done)
	d.SetLazyInitializer(init)

	backend := &notReadyDialer{}
	d.SetBackendDialer(backend)

	conn, err := d.Dial(context.Background(), "tcp", "other.example.com:443")
	require.NoError(t, err)
	conn.Close()

	// No pre-connect override applies, so the dial still falls through to
	// the backend even though it reports not ready: the backend's own
	// reconnect loop owns retries, and dialProxy surfaces its current error.
	assert.Equal(t, []string{"other.example.com:443"}, backend.dialed())
}
