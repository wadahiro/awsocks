package proxy

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wadahiro/awsocks/internal/dns"
	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/mux"
	"github.com/wadahiro/awsocks/internal/routing"
)

var dialerLogger = log.For(log.ComponentProxy)

// UpstreamProxyChecker is optionally implemented by backends that send some
// destinations through an upstream HTTP proxy. Those destinations must keep
// their hostname, because the upstream proxy resolves it and matches its own
// patterns against the name.
type UpstreamProxyChecker interface {
	UsesUpstreamProxy(address string) bool
}

// ProxyDialer handles connection routing, lazy initialization, and fallback logic.
// It is shared between SOCKS5 and HTTP CONNECT proxy servers.
type ProxyDialer struct {
	router          routing.Router
	backend         Dialer
	backendMu       sync.RWMutex
	dialer          Dialer
	agentMux        *mux.AgentMux
	lazyInitializer LazyInitializer
	idleTracker     *IdleTracker
	resolver        *dns.Resolver
}

// NewProxyDialer creates a new ProxyDialer
func NewProxyDialer(router routing.Router, agentMux *mux.AgentMux) *ProxyDialer {
	return &ProxyDialer{
		router:   router,
		agentMux: agentMux,
	}
}

// SetLazyInitializer sets the lazy initializer for deferred AWS initialization
func (d *ProxyDialer) SetLazyInitializer(initializer LazyInitializer) {
	d.lazyInitializer = initializer
}

// SetIdleTracker sets the idle tracker for activity monitoring
func (d *ProxyDialer) SetIdleTracker(tracker *IdleTracker) {
	d.idleTracker = tracker
}

// SetResolver sets the DNS resolver used to override hostname resolution.
// A nil resolver leaves hostnames untouched.
func (d *ProxyDialer) SetResolver(r *dns.Resolver) {
	d.resolver = r
}

// SetBackend updates the backend after lazy initialization
func (d *ProxyDialer) SetBackend(b Dialer) {
	d.backendMu.Lock()
	d.backend = b
	d.backendMu.Unlock()
}

// GetBackend returns the current backend (thread-safe)
func (d *ProxyDialer) GetBackend() Dialer {
	d.backendMu.RLock()
	defer d.backendMu.RUnlock()
	return d.backend
}

// SetBackendDialer sets a simple dialer for testing
func (d *ProxyDialer) SetBackendDialer(dl Dialer) {
	d.backendMu.Lock()
	d.dialer = dl
	d.backendMu.Unlock()
}

// GetDialer returns the current dialer (thread-safe)
func (d *ProxyDialer) GetDialer() Dialer {
	d.backendMu.RLock()
	defer d.backendMu.RUnlock()
	if d.dialer != nil {
		return d.dialer
	}
	return d.backend
}

// isInitialized checks if lazy initialization is complete
func (d *ProxyDialer) isInitialized() bool {
	if d.lazyInitializer == nil {
		return true
	}
	select {
	case <-d.lazyInitializer.InitDone():
		return true
	default:
		return false
	}
}

// Dial is the unified dial function for all route types
func (d *ProxyDialer) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	// Determine route using the original hostname
	route := routing.RouteProxy
	if d.router != nil {
		route = d.router.Route(host)
	}

	// Apply hosts mapping after routing decision (so routing matches on original hostname)
	hostsMapped := false
	if d.router != nil {
		resolved := d.router.ResolveHost(host)
		if resolved != host {
			dialerLogger.Info("Host resolved via hosts mapping", "original", host, "resolved", resolved)
			host = resolved
			hostsMapped = true
			if port != "" {
				addr = net.JoinHostPort(host, port)
			} else {
				addr = host
			}
		}
	}

	// For direct route, connect directly (no need to wait for initialization)
	if route == routing.RouteDirect {
		dialAddr := d.resolveAddr(ctx, host, port, addr, hostsMapped)
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, network, dialAddr)
		if err != nil {
			dialerLogger.Warn("Dial failed", "route", route, "address", dialAddr, "error", err)
		}
		return conn, err
	}

	// For vm-direct route, go through agent (no need to wait for proxy initialization)
	if route == routing.RouteVMDirect {
		dialAddr := d.resolveAddr(ctx, host, port, addr, hostsMapped)
		conn, err := d.dialViaAgent(ctx, network, dialAddr)
		if err != nil {
			dialerLogger.Warn("Dial failed", "route", route, "address", dialAddr, "error", err)
		}
		return conn, err
	}

	// RouteProxy: check if lazy initialization is needed
	if d.lazyInitializer != nil && !d.isInitialized() {
		// Hosts with a pre-connect override skip the wait entirely and use
		// the override route until the backend finishes connecting.
		if d.router != nil {
			if preRoute := d.router.RoutePreConnect(host); preRoute != "" {
				go d.lazyInitializer.EnsureInitialized(context.Background())
				dialerLogger.Info("Backend not ready, using pre-connect route", "address", addr, "route", preRoute)
				return d.dialWithRoute(ctx, network, addr, preRoute)
			}
		}

		// Start initialization in background (non-blocking)
		go d.lazyInitializer.EnsureInitialized(context.Background())

		// Hold proxy connections until initialization completes
		dialerLogger.Info("Waiting for initialization to complete", "address", addr)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-d.lazyInitializer.InitDone():
			if err := d.lazyInitializer.InitError(); err != nil {
				return nil, fmt.Errorf("initialization failed: %w", err)
			}
			dialerLogger.Info("Initialization complete, dialing via proxy", "address", addr)
		}
	}

	// Resolve only now that the backend is ready: a rule with via=proxy sends
	// its query through this same backend, so resolving earlier would block on
	// the initialization this call is already waiting for.
	dialAddr := addr
	resolved := false
	if !d.skipDNSResolve(addr) {
		if a := d.resolveAddr(ctx, host, port, addr, hostsMapped); a != addr {
			dialAddr = a
			resolved = true
		}
	}

	// Try primary route (proxy)
	conn, err := d.dialProxy(ctx, network, dialAddr)
	if err == nil {
		if d.idleTracker != nil {
			d.idleTracker.Touch()
		}
		return conn, nil
	}

	// Check if fallback is needed
	if !routing.IsFallbackableError(err) {
		dialerLogger.Warn("Dial failed", "route", route, "address", dialAddr, "error", err)
		return nil, err
	}

	// The resolved address turned out to be unreachable. Drop it so a changed
	// address (failover, rescheduling) is picked up instead of being served
	// from cache until the TTL expires.
	if resolved {
		d.resolver.Invalidate(host)
	}

	// Get fallback route
	fallbackRoute := d.router.FallbackRoute(route)
	if fallbackRoute == "" {
		return nil, err
	}

	dialerLogger.Info("Fallback to alternative route",
		"address", addr, "from", route, "to", fallbackRoute, "reason", err)

	fallbackConn, fallbackErr := d.dialWithRoute(ctx, network, addr, fallbackRoute)
	if fallbackErr != nil {
		dialerLogger.Warn("Fallback dial failed", "route", fallbackRoute, "address", addr, "error", fallbackErr)
	} else if fallbackRoute == routing.RouteProxy && d.idleTracker != nil {
		d.idleTracker.Touch()
	}
	return fallbackConn, fallbackErr
}

// resolveAddr returns the address to dial for host, applying the configured
// DNS rules. It returns addr unchanged when no rule applies, when a static
// hosts entry already decided the address, or when resolution failed under the
// fallthrough policy.
func (d *ProxyDialer) resolveAddr(ctx context.Context, host, port, addr string, hostsMapped bool) string {
	// An explicit hosts entry is a stronger statement than a DNS answer,
	// matching how /etc/hosts takes precedence over a resolver.
	if hostsMapped || !d.resolver.Enabled() {
		return addr
	}

	ip, ok, err := d.resolver.Resolve(ctx, host)
	if err != nil {
		// Only a rule with on-failure=fail produces an error here. Log it and
		// keep the hostname; the dial that follows will surface the real
		// failure with more context than an unresolvable name would.
		dialerLogger.Warn("DNS resolution failed", "host", host, "error", err)
		return addr
	}
	if !ok {
		return addr
	}

	if port == "" {
		return ip.String()
	}
	return net.JoinHostPort(ip.String(), port)
}

// skipDNSResolve reports whether the backend sends this address through an
// upstream proxy, which needs the hostname rather than an address.
func (d *ProxyDialer) skipDNSResolve(addr string) bool {
	checker, ok := d.GetDialer().(UpstreamProxyChecker)
	return ok && checker.UsesUpstreamProxy(addr)
}

// dialProxy dials using the proxy backend
func (d *ProxyDialer) dialProxy(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := d.GetDialer()
	if dialer != nil {
		return dialer.Dial(ctx, network, addr)
	}
	// No backend configured, fall back to direct
	var netDialer net.Dialer
	return netDialer.DialContext(ctx, network, addr)
}

// dialWithRoute dials using the specified route
func (d *ProxyDialer) dialWithRoute(ctx context.Context, network, addr string, route routing.Route) (net.Conn, error) {
	switch route {
	case routing.RouteDirect:
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, addr)
	case routing.RouteVMDirect:
		return d.dialViaAgent(ctx, network, addr)
	default:
		return d.dialProxy(ctx, network, addr)
	}
}

// dialViaAgent dials via the VM agent using the shared AgentMux
func (d *ProxyDialer) dialViaAgent(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.agentMux == nil {
		return nil, fmt.Errorf("no agent connection available for vm-direct route")
	}

	dialCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}

	return d.agentMux.Dial(dialCtx, network, addr)
}
