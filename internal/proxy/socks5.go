// Package proxy implements SOCKS5 proxy server
package proxy

import (
	"context"
	"fmt"
	golog "log"
	"net"
	"strings"
	"sync"

	gosocks5 "github.com/armon/go-socks5"
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
	cfg        *Config
	dialer     *ProxyDialer
	ctx        context.Context
	cancel     context.CancelFunc
	listener   net.Listener
	listenerMu sync.Mutex
}

// NewSOCKS5Server creates a new unified SOCKS5 server
func NewSOCKS5Server(cfg *Config, router routing.Router, agentMux *mux.AgentMux) *SOCKS5Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &SOCKS5Server{
		cfg:    cfg,
		dialer: NewProxyDialer(router, agentMux),
		ctx:    ctx,
		cancel: cancel,
	}
	return s
}

// Dialer returns the underlying ProxyDialer for shared access
func (s *SOCKS5Server) Dialer() *ProxyDialer {
	return s.dialer
}

// SetLazyInitializer sets the lazy initializer for deferred AWS initialization
func (s *SOCKS5Server) SetLazyInitializer(initializer LazyInitializer) {
	s.dialer.SetLazyInitializer(initializer)
}

// SetIdleTracker sets the idle tracker for activity monitoring
func (s *SOCKS5Server) SetIdleTracker(tracker *IdleTracker) {
	s.dialer.SetIdleTracker(tracker)
}

// SetBackend updates the backend after lazy initialization
func (s *SOCKS5Server) SetBackend(b Dialer) {
	s.dialer.SetBackend(b)
}

// GetBackend returns the current backend (thread-safe)
func (s *SOCKS5Server) GetBackend() Dialer {
	return s.dialer.GetBackend()
}

// SetBackendDialer sets a simple dialer for testing
func (s *SOCKS5Server) SetBackendDialer(d Dialer) {
	s.dialer.SetBackendDialer(d)
}

// GetDialer returns the current dialer (thread-safe)
func (s *SOCKS5Server) GetDialer() Dialer {
	return s.dialer.GetDialer()
}

// Start starts the unified SOCKS5 server
func (s *SOCKS5Server) Start() error {
	conf := &gosocks5.Config{
		Dial:     s.dialer.Dial,
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

// dial is exposed for backward compatibility with tests
func (s *SOCKS5Server) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return s.dialer.Dial(ctx, network, addr)
}
