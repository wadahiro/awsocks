package routing

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRoute_IsValid(t *testing.T) {
	tests := []struct {
		name  string
		route Route
		want  bool
	}{
		{"proxy", RouteProxy, true},
		{"direct", RouteDirect, true},
		{"vm-direct", RouteVMDirect, true},
		{"invalid", Route("invalid"), false},
		{"empty", Route(""), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.route.IsValid())
		})
	}
}

func TestDomainMatcher(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		host    string
		want    bool
	}{
		// Exact match
		{"exact match", "example.com", "example.com", true},
		{"exact match case insensitive", "Example.Com", "example.com", true},
		{"exact no match", "example.com", "other.com", false},
		{"exact no match subdomain", "example.com", "sub.example.com", false},

		// Wildcard suffix match
		{"wildcard suffix match", "*.example.com", "sub.example.com", true},
		{"wildcard suffix match deep", "*.example.com", "a.b.example.com", true},
		{"wildcard suffix match exact domain", "*.example.com", "example.com", true},
		{"wildcard no match", "*.example.com", "other.com", false},
		{"wildcard no match partial", "*.example.com", "notexample.com", false},

		// Special cases
		{"localhost", "localhost", "localhost", true},
		{"localhost no match", "localhost", "localhost.local", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewDomainMatcher(tt.pattern)
			assert.Equal(t, tt.want, m.Match(tt.host))
		})
	}
}

func TestCIDRMatcher(t *testing.T) {
	tests := []struct {
		name    string
		cidr    string
		host    string
		want    bool
	}{
		// IPv4
		{"ipv4 in range", "10.0.0.0/8", "10.1.2.3", true},
		{"ipv4 not in range", "10.0.0.0/8", "192.168.1.1", false},
		{"ipv4 in small range", "192.168.1.0/24", "192.168.1.100", true},
		{"ipv4 not in small range", "192.168.1.0/24", "192.168.2.100", false},
		{"ipv4 exact match", "192.168.1.1/32", "192.168.1.1", true},
		{"ipv4 exact no match", "192.168.1.1/32", "192.168.1.2", false},

		// Non-IP addresses
		{"hostname no match", "10.0.0.0/8", "example.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := NewCIDRMatcher(tt.cidr)
			assert.NoError(t, err)
			assert.Equal(t, tt.want, m.Match(tt.host))
		})
	}
}

func TestExactMatcher(t *testing.T) {
	tests := []struct {
		name  string
		value string
		host  string
		want  bool
	}{
		{"exact match", "localhost", "localhost", true},
		{"case insensitive", "LOCALHOST", "localhost", true},
		{"no match", "localhost", "other", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewExactMatcher(tt.value)
			assert.Equal(t, tt.want, m.Match(tt.host))
		})
	}
}

func TestParseMatcher(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		host    string
		want    bool
	}{
		// CIDR patterns
		{"cidr", "10.0.0.0/8", "10.1.2.3", true},
		{"cidr no match", "10.0.0.0/8", "192.168.1.1", false},

		// Single IP (converted to /32)
		{"single ip", "192.168.1.1", "192.168.1.1", true},
		{"single ip no match", "192.168.1.1", "192.168.1.2", false},

		// Domain patterns
		{"wildcard domain", "*.example.com", "sub.example.com", true},
		{"exact domain", "example.com", "example.com", true},

		// Special values
		{"localhost", "localhost", "localhost", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := ParseMatcher(tt.pattern)
			assert.Equal(t, tt.want, m.Match(tt.host))
		})
	}
}

func TestRouter_DirectMode(t *testing.T) {
	cfg := &Config{
		Default: "proxy",
		Proxy: []string{
			"*.internal.company.com",
			"10.0.0.0/8",
		},
		Direct: []string{
			"localhost",
			"127.0.0.0/8",
			"*.local",
		},
	}

	router := NewRouter(cfg)

	tests := []struct {
		name string
		host string
		want Route
	}{
		// Direct patterns
		{"localhost", "localhost", RouteDirect},
		{"loopback", "127.0.0.1", RouteDirect},
		{"local domain", "myhost.local", RouteDirect},

		// Proxy patterns
		{"internal domain", "api.internal.company.com", RouteProxy},
		{"private ip", "10.1.2.3", RouteProxy},

		// Default to proxy
		{"external domain", "www.google.com", RouteProxy},
		{"public ip", "8.8.8.8", RouteProxy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, router.Route(tt.host))
		})
	}
}

func TestRouter_VMMode(t *testing.T) {
	cfg := &Config{
		Default: "proxy",
		Proxy: []string{
			"*.internal.company.com",
			"10.0.0.0/8",
		},
		Direct: []string{
			"localhost",
			"127.0.0.0/8",
		},
		VMDirect: []string{
			"*.google.com",
			"*.github.com",
		},
	}

	router := NewRouter(cfg, WithVMMode())

	tests := []struct {
		name string
		host string
		want Route
	}{
		// VM-direct patterns (highest priority)
		{"google", "www.google.com", RouteVMDirect},
		{"github", "api.github.com", RouteVMDirect},

		// Direct patterns
		{"localhost", "localhost", RouteDirect},
		{"loopback", "127.0.0.1", RouteDirect},

		// Proxy patterns
		{"internal domain", "api.internal.company.com", RouteProxy},
		{"private ip", "10.1.2.3", RouteProxy},

		// Default to proxy
		{"other external", "www.amazon.com", RouteProxy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, router.Route(tt.host))
		})
	}
}

func TestRouter_VMDirectIgnoredInDirectMode(t *testing.T) {
	cfg := &Config{
		Default: "proxy",
		VMDirect: []string{
			"*.google.com",
		},
	}

	// Without WithVMMode(), vm-direct patterns should be ignored
	router := NewRouter(cfg)

	// Should fall back to default (proxy) since vm-direct is not enabled
	assert.Equal(t, RouteProxy, router.Route("www.google.com"))
}

func TestRouter_DefaultDirect(t *testing.T) {
	cfg := &Config{
		Default: "direct",
		Proxy: []string{
			"*.internal.company.com",
		},
	}

	router := NewRouter(cfg)

	tests := []struct {
		name string
		host string
		want Route
	}{
		// Proxy patterns
		{"internal domain", "api.internal.company.com", RouteProxy},

		// Default to direct
		{"external domain", "www.google.com", RouteDirect},
		{"any ip", "8.8.8.8", RouteDirect},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, router.Route(tt.host))
		})
	}
}

func TestDefaultRouter(t *testing.T) {
	router := NewDefaultRouter()

	// Default router should route everything to proxy
	assert.Equal(t, RouteProxy, router.Route("localhost"))
	assert.Equal(t, RouteProxy, router.Route("www.google.com"))
	assert.Equal(t, RouteProxy, router.Route("10.0.0.1"))
}

func TestDefaultRouter_WithVMMode(t *testing.T) {
	router := NewDefaultRouter(WithVMMode())

	// Default router with VM mode should route everything to proxy
	assert.Equal(t, RouteProxy, router.Route("localhost"))
	assert.Equal(t, RouteProxy, router.Route("www.google.com"))
	assert.Equal(t, RouteProxy, router.Route("10.0.0.1"))

	// But FallbackRoute should return vm-direct instead of direct
	assert.Equal(t, RouteVMDirect, router.FallbackRoute(RouteProxy))
	assert.Equal(t, RouteProxy, router.FallbackRoute(RouteVMDirect))
}

func TestDefaultRouter_WithoutVMMode_FallbackRoute(t *testing.T) {
	router := NewDefaultRouter()

	// Without VM mode, FallbackRoute should return direct
	assert.Equal(t, RouteDirect, router.FallbackRoute(RouteProxy))
	assert.Equal(t, RouteProxy, router.FallbackRoute(RouteDirect))
}
