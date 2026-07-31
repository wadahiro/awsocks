package proxy

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/mux"
	"github.com/wadahiro/awsocks/internal/routing"
)

var dialerLogger = log.For(log.ComponentProxy)

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
	if d.router != nil {
		resolved := d.router.ResolveHost(host)
		if resolved != host {
			dialerLogger.Info("Host resolved via hosts mapping", "original", host, "resolved", resolved)
			host = resolved
			if port != "" {
				addr = net.JoinHostPort(host, port)
			} else {
				addr = host
			}
		}
	}

	// For direct route, connect directly (no need to wait for initialization)
	if route == routing.RouteDirect {
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			dialerLogger.Warn("Dial failed", "route", route, "address", addr, "error", err)
		}
		return conn, err
	}

	// For vm-direct route, go through agent (no need to wait for proxy initialization)
	if route == routing.RouteVMDirect {
		conn, err := d.dialViaAgent(ctx, network, addr)
		if err != nil {
			dialerLogger.Warn("Dial failed", "route", route, "address", addr, "error", err)
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

	// Try primary route (proxy)
	conn, err := d.dialProxy(ctx, network, addr)
	if err == nil {
		if d.idleTracker != nil {
			d.idleTracker.Touch()
		}
		return conn, nil
	}

	// Check if fallback is needed
	if !routing.IsFallbackableError(err) {
		dialerLogger.Warn("Dial failed", "route", route, "address", addr, "error", err)
		return nil, err
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
