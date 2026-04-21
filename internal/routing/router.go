package routing

import (
	"strings"

	"github.com/wadahiro/awsocks/internal/log"
)

var logger = log.For(log.ComponentRouting)

// Router determines the routing path for a given destination
type Router interface {
	// Route determines the route for the given host
	Route(host string) Route
	// FallbackRoute returns the fallback route for the given route.
	// Returns empty string if no fallback is available.
	FallbackRoute(current Route) Route
	// ResolveHost returns the mapped address for the given host.
	// If no mapping exists, returns the original host unchanged.
	ResolveHost(host string) string
}

// DefaultRouter implements Router with pattern-based routing rules
type DefaultRouter struct {
	defaultRoute     Route
	proxyMatchers    []Matcher
	directMatchers   []Matcher
	vmDirectMatchers []Matcher
	vmModeEnabled    bool
	hosts            map[string]string // lowercase hostname -> address
}

// RouterOption configures the router
type RouterOption func(*DefaultRouter)

// WithVMMode enables VM mode routing (vm-direct route)
func WithVMMode() RouterOption {
	return func(r *DefaultRouter) {
		r.vmModeEnabled = true
	}
}

// NewRouter creates a new router from configuration
func NewRouter(cfg *Config, opts ...RouterOption) *DefaultRouter {
	r := &DefaultRouter{
		defaultRoute:   Route(cfg.Default),
	}

	for _, opt := range opts {
		opt(r)
	}

	// Parse proxy patterns
	for _, pattern := range cfg.Proxy {
		r.proxyMatchers = append(r.proxyMatchers, ParseMatcher(pattern))
	}

	// Parse direct patterns
	for _, pattern := range cfg.Direct {
		r.directMatchers = append(r.directMatchers, ParseMatcher(pattern))
	}

	// Parse vm-direct patterns (only relevant in VM mode)
	if r.vmModeEnabled {
		for _, pattern := range cfg.VMDirect {
			r.vmDirectMatchers = append(r.vmDirectMatchers, ParseMatcher(pattern))
		}
	}

	// Build hosts mapping (normalized to lowercase)
	if len(cfg.Hosts) > 0 {
		r.hosts = make(map[string]string, len(cfg.Hosts))
		for hostname, addr := range cfg.Hosts {
			r.hosts[strings.ToLower(hostname)] = addr
		}
	}

	return r
}

// NewDefaultRouter creates a router with default (proxy-only) routing
func NewDefaultRouter(opts ...RouterOption) *DefaultRouter {
	r := &DefaultRouter{
		defaultRoute: RouteProxy,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Route determines the route for the given host
func (r *DefaultRouter) Route(host string) Route {
	// Check vm-direct patterns first (most specific in VM mode)
	if r.vmModeEnabled {
		for _, m := range r.vmDirectMatchers {
			if m.Match(host) {
				logger.Debug("Route matched", "host", host, "route", RouteVMDirect)
				return RouteVMDirect
			}
		}
	}

	// Check direct patterns
	for _, m := range r.directMatchers {
		if m.Match(host) {
			logger.Debug("Route matched", "host", host, "route", RouteDirect)
			return RouteDirect
		}
	}

	// Check proxy patterns
	for _, m := range r.proxyMatchers {
		if m.Match(host) {
			logger.Debug("Route matched", "host", host, "route", RouteProxy)
			return RouteProxy
		}
	}

	// Use default route
	logger.Debug("Route default", "host", host, "route", r.defaultRoute)
	return r.defaultRoute
}

// DefaultRoute returns the default route
func (r *DefaultRouter) DefaultRoute() Route {
	return r.defaultRoute
}

// ResolveHost returns the mapped address for the given host.
// If no mapping exists, returns the original host unchanged.
func (r *DefaultRouter) ResolveHost(host string) string {
	if len(r.hosts) == 0 {
		return host
	}
	if addr, ok := r.hosts[strings.ToLower(host)]; ok {
		logger.Debug("Host resolved via hosts mapping", "host", host, "resolved", addr)
		return addr
	}
	return host
}

// HasVMDirectRoutes returns true if any vm-direct routes are configured
func (r *DefaultRouter) HasVMDirectRoutes() bool {
	return len(r.vmDirectMatchers) > 0
}

// FallbackRoute returns the fallback route for the given route.
// Returns empty string if no fallback is available.
//
// In VM mode:
//   - proxy and vm-direct are mutual fallbacks (both are trusted routes)
//   - direct has no fallback (explicit user configuration, safety-first)
//
// In Direct mode:
//   - proxy and direct are mutual fallbacks
func (r *DefaultRouter) FallbackRoute(current Route) Route {
	if r.vmModeEnabled {
		// VM mode: proxy and vm-direct are mutual fallbacks
		// direct is explicit-only for security
		switch current {
		case RouteProxy:
			return RouteVMDirect
		case RouteVMDirect:
			return RouteProxy
		case RouteDirect:
			return "" // No fallback for explicit direct
		}
	} else {
		// Direct mode: proxy and direct are mutual fallbacks
		switch current {
		case RouteProxy:
			return RouteDirect
		case RouteDirect:
			return RouteProxy
		}
	}
	return ""
}
