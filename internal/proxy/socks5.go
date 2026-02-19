// Package proxy implements SOCKS5 proxy server
package proxy

import (
	"context"
	"fmt"
	golog "log"
	"net"
	"strings"
	"sync"
	"time"

	gosocks5 "github.com/armon/go-socks5"
	"github.com/wadahiro/awsocks/internal/backend"
	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/mux"
	"github.com/wadahiro/awsocks/internal/routing"
)

var proxyLogger = log.For(log.ComponentProxy)

// slogWriter adapts slog to io.Writer for go-socks5 Logger
type slogWriter struct{}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	// go-socks5 uses "[ERR]" prefix for errors
	if strings.HasPrefix(msg, "[ERR]") {
		msg = strings.TrimPrefix(msg, "[ERR] ")
		// Downgrade client disconnect errors to DEBUG (normal behavior)
		if isClientDisconnectError(msg) {
			proxyLogger.Debug(msg)
		} else if isDialError(msg) {
			// Dial failures are already logged with route info in dial()
			proxyLogger.Debug(msg)
		} else {
			proxyLogger.Error(msg)
		}
	} else {
		proxyLogger.Debug(msg)
	}
	return len(p), nil
}

// isClientDisconnectError checks if the error is a normal client disconnect
func isClientDisconnectError(msg string) bool {
	// These errors occur when client closes connection before proxy finishes
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "read timeout") ||
		strings.Contains(msg, "write timeout") ||
		strings.Contains(msg, "i/o timeout")
}

// isDialError checks if the error is a dial failure (already logged with route info)
func isDialError(msg string) bool {
	return strings.Contains(msg, "Connect to") && strings.Contains(msg, "failed:")
}

// LazyInitializer is an interface for triggering lazy initialization
type LazyInitializer interface {
	EnsureInitialized(ctx context.Context) error
	InitDone() <-chan struct{} // closed when initialization completes (success or failure)
	InitError() error          // returns error if initialization failed
}

// noopResolver is a NameResolver that does not resolve hostnames
// It returns a nil IP so that the Dial function receives the original hostname
type noopResolver struct{}

func (r *noopResolver) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	return ctx, nil, nil
}

// SOCKS5Server provides a unified SOCKS5 proxy that handles all route types.
// - RouteDirect: connects directly from host via net.Dialer
// - RouteVMDirect: connects via AgentMux using MsgConnectDirect protocol
// - RouteProxy: connects via SSM backend's Dial()
type SOCKS5Server struct {
	cfg             *Config
	router          routing.Router
	backend         backend.Backend // proxy route (may be nil)
	backendMu       sync.RWMutex
	dialer          Dialer // For testing without full backend
	agentMux        *mux.AgentMux  // shared multiplexer (may be nil)
	ctx             context.Context
	cancel          context.CancelFunc
	listener        net.Listener
	listenerMu      sync.Mutex
	lazyInitializer LazyInitializer
	idleTracker     *IdleTracker
}

// NewSOCKS5Server creates a new unified SOCKS5 server
func NewSOCKS5Server(cfg *Config, router routing.Router, agentMux *mux.AgentMux) *SOCKS5Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &SOCKS5Server{
		cfg:      cfg,
		router:   router,
		agentMux: agentMux,
		ctx:      ctx,
		cancel:   cancel,
	}
	return s
}

// SetLazyInitializer sets the lazy initializer for deferred AWS initialization
func (s *SOCKS5Server) SetLazyInitializer(initializer LazyInitializer) {
	s.lazyInitializer = initializer
}

// SetIdleTracker sets the idle tracker for activity monitoring
func (s *SOCKS5Server) SetIdleTracker(tracker *IdleTracker) {
	s.idleTracker = tracker
}

// SetBackend updates the backend after lazy initialization
func (s *SOCKS5Server) SetBackend(b backend.Backend) {
	s.backendMu.Lock()
	s.backend = b
	s.backendMu.Unlock()
}

// GetBackend returns the current backend (thread-safe)
func (s *SOCKS5Server) GetBackend() backend.Backend {
	s.backendMu.RLock()
	defer s.backendMu.RUnlock()
	return s.backend
}

// SetBackendDialer sets a simple dialer for testing
func (s *SOCKS5Server) SetBackendDialer(d Dialer) {
	s.backendMu.Lock()
	s.dialer = d
	s.backendMu.Unlock()
}

// GetDialer returns the current dialer (thread-safe)
func (s *SOCKS5Server) GetDialer() Dialer {
	s.backendMu.RLock()
	defer s.backendMu.RUnlock()
	if s.dialer != nil {
		return s.dialer
	}
	return s.backend
}

// Start starts the unified SOCKS5 server
func (s *SOCKS5Server) Start() error {
	conf := &gosocks5.Config{
		Dial:     s.dial,
		Resolver: &noopResolver{},
		Logger:   golog.New(&slogWriter{}, "", 0),
	}

	server, err := gosocks5.New(conf)
	if err != nil {
		return fmt.Errorf("failed to create SOCKS5 server: %w", err)
	}

	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.cfg.ListenAddr, err)
	}

	s.listenerMu.Lock()
	s.listener = listener
	s.listenerMu.Unlock()

	return server.Serve(listener)
}

// Stop stops the SOCKS5 server
func (s *SOCKS5Server) Stop() {
	s.cancel()
	s.listenerMu.Lock()
	if s.listener != nil {
		s.listener.Close()
	}
	s.listenerMu.Unlock()
}

// isInitialized checks if lazy initialization is complete
func (s *SOCKS5Server) isInitialized() bool {
	if s.lazyInitializer == nil {
		return true
	}
	select {
	case <-s.lazyInitializer.InitDone():
		return true
	default:
		return false
	}
}

// dial is the unified dial function for all route types
func (s *SOCKS5Server) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	// Determine route
	route := routing.RouteProxy
	if s.router != nil {
		route = s.router.Route(host)
	}

	// For direct route, connect directly (no need to wait for initialization)
	if route == routing.RouteDirect {
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			proxyLogger.Warn("Dial failed", "route", route, "address", addr, "error", err)
		}
		return conn, err
	}

	// For vm-direct route, go through agent (no need to wait for proxy initialization)
	if route == routing.RouteVMDirect {
		conn, err := s.dialViaAgent(ctx, network, addr)
		if err != nil {
			proxyLogger.Warn("Dial failed", "route", route, "address", addr, "error", err)
		}
		return conn, err
	}

	// RouteProxy: check if lazy initialization is needed
	if s.lazyInitializer != nil && !s.isInitialized() {
		// Start initialization in background (non-blocking)
		go s.lazyInitializer.EnsureInitialized(context.Background())

		// Hold proxy connections until initialization completes
		proxyLogger.Info("Waiting for initialization to complete", "address", addr)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.lazyInitializer.InitDone():
			if err := s.lazyInitializer.InitError(); err != nil {
				return nil, fmt.Errorf("initialization failed: %w", err)
			}
			proxyLogger.Info("Initialization complete, dialing via proxy", "address", addr)
		}
	}

	// Try primary route (proxy)
	conn, err := s.dialProxy(ctx, network, addr)
	if err == nil {
		if s.idleTracker != nil {
			s.idleTracker.Touch()
		}
		return conn, nil
	}

	// Check if fallback is needed
	if !routing.IsFallbackableError(err) {
		proxyLogger.Warn("Dial failed", "route", route, "address", addr, "error", err)
		return nil, err
	}

	// Get fallback route
	fallbackRoute := s.router.FallbackRoute(route)
	if fallbackRoute == "" {
		return nil, err
	}

	proxyLogger.Info("Fallback to alternative route",
		"address", addr, "from", route, "to", fallbackRoute, "reason", err)

	fallbackConn, fallbackErr := s.dialWithRoute(ctx, network, addr, fallbackRoute)
	if fallbackErr != nil {
		proxyLogger.Warn("Fallback dial failed", "route", fallbackRoute, "address", addr, "error", fallbackErr)
	} else if fallbackRoute == routing.RouteProxy && s.idleTracker != nil {
		s.idleTracker.Touch()
	}
	return fallbackConn, fallbackErr
}

// dialProxy dials using the proxy backend
func (s *SOCKS5Server) dialProxy(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := s.GetDialer()
	if dialer != nil {
		return dialer.Dial(ctx, network, addr)
	}
	// No backend configured, fall back to direct
	var netDialer net.Dialer
	return netDialer.DialContext(ctx, network, addr)
}

// dialWithRoute dials using the specified route
func (s *SOCKS5Server) dialWithRoute(ctx context.Context, network, addr string, route routing.Route) (net.Conn, error) {
	switch route {
	case routing.RouteDirect:
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, addr)
	case routing.RouteVMDirect:
		return s.dialViaAgent(ctx, network, addr)
	default:
		return s.dialProxy(ctx, network, addr)
	}
}

// dialViaAgent dials via the VM agent using the shared AgentMux
func (s *SOCKS5Server) dialViaAgent(ctx context.Context, network, addr string) (net.Conn, error) {
	if s.agentMux == nil {
		return nil, fmt.Errorf("no agent connection available for vm-direct route")
	}

	dialCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}

	return s.agentMux.Dial(dialCtx, network, addr)
}
