// Package proxy implements SOCKS5 proxy server
package proxy

import (
	"context"
	"fmt"
	"io"
	golog "log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gosocks5 "github.com/armon/go-socks5"
	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/protocol"
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
		strings.Contains(msg, "use of closed network connection")
}

// connResult holds the result of a connection attempt
type connResult struct {
	conn net.Conn
	err  error
}

// LazyInitializer is an interface for triggering lazy initialization
type LazyInitializer interface {
	EnsureInitialized(ctx context.Context) error
}

// SOCKS5Server provides a SOCKS5 proxy that forwards connections via vsock to VM agent
type SOCKS5Server struct {
	listenAddr      string
	agentConn       net.Conn
	router          routing.Router
	connMu          sync.Mutex
	nextConnID      uint32
	pending         map[uint32]chan connResult
	pendingMu       sync.Mutex
	ctx             context.Context
	cancel          context.CancelFunc
	listener        net.Listener
	listenerMu      sync.Mutex
	lazyInitializer LazyInitializer
}

// NewSOCKS5Server creates a new SOCKS5 server
func NewSOCKS5Server(listenAddr string, agentConn net.Conn, router routing.Router) *SOCKS5Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &SOCKS5Server{
		listenAddr: listenAddr,
		agentConn:  agentConn,
		router:     router,
		pending:    make(map[uint32]chan connResult),
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Start starts the SOCKS5 server
func (s *SOCKS5Server) Start() error {
	// Start message reader from agent
	go s.readFromAgent()

	// Create SOCKS5 server with custom dialer and no DNS resolution
	// noopResolver ensures hostnames are passed to the VM agent without local resolution
	conf := &gosocks5.Config{
		Dial:     s.dialViaAgent,
		Resolver: &noopResolver{},
		Logger:   golog.New(&slogWriter{}, "", 0),
	}

	server, err := gosocks5.New(conf)
	if err != nil {
		return fmt.Errorf("failed to create SOCKS5 server: %w", err)
	}

	// Create listener manually so we can close it on Stop()
	listener, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.listenAddr, err)
	}

	s.listenerMu.Lock()
	s.listener = listener
	s.listenerMu.Unlock()

	proxyLogger.Info("Starting SOCKS5 server", "listen", s.listenAddr)
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

// SetVMManager sets the lazy initializer for deferred AWS initialization
func (s *SOCKS5Server) SetVMManager(initializer LazyInitializer) {
	s.lazyInitializer = initializer
}

// dialViaAgent establishes a connection through the VM agent
func (s *SOCKS5Server) dialViaAgent(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	// Determine route
	route := routing.RouteProxy
	if s.router != nil {
		route = s.router.Route(host)
	}

	// Handle direct route from host (bypass VM entirely)
	if route == routing.RouteDirect {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, addr)
	}

	// Check if lazy initialization is needed
	if s.lazyInitializer != nil && !s.isInitialized() {
		// Start initialization in background (non-blocking)
		go s.lazyInitializer.EnsureInitialized(context.Background())

		// During initialization, use vm-direct to avoid blocking
		// This allows OIDC auth flow and other requests to proceed
		proxyLogger.Debug("Initialization in progress, using vm-direct", "address", addr)
		return s.dialWithRoute(ctx, network, addr, routing.RouteVMDirect)
	}

	// Try primary route
	conn, err := s.dialWithRoute(ctx, network, addr, route)
	if err == nil {
		return conn, nil
	}

	// Check if fallback is needed
	if !routing.IsFallbackableError(err) {
		return nil, err
	}

	// Get fallback route
	fallbackRoute := s.router.FallbackRoute(route)
	if fallbackRoute == "" {
		return nil, err // No fallback available
	}

	proxyLogger.Info("Fallback to alternative route",
		"address", addr, "from", route, "to", fallbackRoute, "reason", err)

	return s.dialWithRoute(ctx, network, addr, fallbackRoute)
}

// isInitialized checks if lazy initialization is complete
func (s *SOCKS5Server) isInitialized() bool {
	if s.lazyInitializer == nil {
		return true
	}
	// Check if VMManager is initialized
	if vmMgr, ok := s.lazyInitializer.(*VMManager); ok {
		vmMgr.awsInitMu.Lock()
		defer vmMgr.awsInitMu.Unlock()
		return vmMgr.awsInitialized
	}
	return true
}

// dialWithRoute attempts to dial using the specified route
func (s *SOCKS5Server) dialWithRoute(ctx context.Context, network, addr string, route routing.Route) (net.Conn, error) {
	// Handle direct route from host (bypass VM entirely)
	if route == routing.RouteDirect {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, addr)
	}

	connID := atomic.AddUint32(&s.nextConnID, 1)

	// Create response channel (buffered for conn or error)
	respCh := make(chan connResult, 1)
	s.pendingMu.Lock()
	s.pending[connID] = respCh
	s.pendingMu.Unlock()

	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, connID)
		s.pendingMu.Unlock()
	}()

	// Send connect request to agent
	var msg *protocol.Message
	if route == routing.RouteVMDirect {
		// VM NAT direct connection (bypass EC2)
		msg = protocol.NewConnectDirectMessage(connID, network, addr)
	} else {
		// RouteProxy: go through EC2
		msg = protocol.NewConnectMessage(connID, network, addr)
	}

	s.connMu.Lock()
	err := protocol.WriteMessage(s.agentConn, msg)
	s.connMu.Unlock()

	if err != nil {
		return nil, fmt.Errorf("failed to send connect request: %w", err)
	}

	// Wait for response with timeout
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("connection timeout")
	case result := <-respCh:
		if result.conn == nil {
			return nil, result.err
		}
		return result.conn, nil
	}
}

// readFromAgent reads messages from the VM agent
func (s *SOCKS5Server) readFromAgent() {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		msg, err := protocol.ReadMessage(s.agentConn)
		if err != nil {
			if err != io.EOF {
				proxyLogger.Error("Error reading from agent", "error", err)
			}
			return
		}

		switch msg.Type {
		case protocol.MsgConnectAck:
			s.handleConnectAck(msg)
		case protocol.MsgData:
			s.handleData(msg)
		case protocol.MsgClose:
			s.handleClose(msg)
		case protocol.MsgError:
			s.handleError(msg)
		case protocol.MsgLog:
			s.handleLog(msg)
		case protocol.MsgPong:
			// Ignore pong
		default:
			proxyLogger.Warn("Unknown message type", "type", msg.Type)
		}
	}
}

func (s *SOCKS5Server) handleConnectAck(msg *protocol.Message) {
	s.pendingMu.Lock()
	respCh, ok := s.pending[msg.ConnID]
	s.pendingMu.Unlock()

	if !ok {
		proxyLogger.Warn("ConnectAck for unknown connection", "connID", msg.ConnID)
		return
	}

	// Create virtual connection
	conn := newVirtualConn(msg.ConnID, s)
	respCh <- connResult{conn: conn}
}

func (s *SOCKS5Server) handleData(msg *protocol.Message) {
	// Route data to the appropriate virtual connection
	// This is handled by the virtualConn's read buffer
	connMgr.deliverData(msg.ConnID, msg.Payload)
}

func (s *SOCKS5Server) handleClose(msg *protocol.Message) {
	connMgr.closeConn(msg.ConnID)
}

func (s *SOCKS5Server) handleError(msg *protocol.Message) {
	errMsg := string(msg.Payload)
	proxyLogger.Debug("Error for connection", "connID", msg.ConnID, "error", errMsg)

	s.pendingMu.Lock()
	respCh, ok := s.pending[msg.ConnID]
	s.pendingMu.Unlock()

	if ok {
		// Send error with message for fallback detection
		respCh <- connResult{err: fmt.Errorf("%s", errMsg)}
	}

	connMgr.closeConn(msg.ConnID)
}

func (s *SOCKS5Server) handleLog(msg *protocol.Message) {
	logPayload, err := protocol.ParseLogPayload(msg.Payload)
	if err != nil {
		proxyLogger.Warn("Failed to parse log message", "error", err)
		return
	}

	// Use agent logger to display agent logs
	agentLogger := log.For(log.ComponentAgent)
	switch logPayload.Level {
	case "debug":
		agentLogger.Debug(logPayload.Message)
	case "info":
		agentLogger.Info(logPayload.Message)
	case "warn":
		agentLogger.Warn(logPayload.Message)
	case "error":
		agentLogger.Error(logPayload.Message)
	default:
		agentLogger.Info(logPayload.Message)
	}
}

// sendToAgent sends a message to the VM agent
func (s *SOCKS5Server) sendToAgent(msg *protocol.Message) error {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return protocol.WriteMessage(s.agentConn, msg)
}

// virtualConn implements net.Conn for proxied connections
type virtualConn struct {
	id        uint32
	server    *SOCKS5Server
	readBuf   chan []byte
	closed    bool
	closeMu   sync.Mutex
	closeOnce sync.Once
}

// Global connection manager
var connMgr = &connectionManager{
	conns: make(map[uint32]*virtualConn),
}

type connectionManager struct {
	conns map[uint32]*virtualConn
	mu    sync.RWMutex
}

func (m *connectionManager) register(conn *virtualConn) {
	m.mu.Lock()
	m.conns[conn.id] = conn
	m.mu.Unlock()
}

func (m *connectionManager) unregister(id uint32) {
	m.mu.Lock()
	delete(m.conns, id)
	m.mu.Unlock()
}

func (m *connectionManager) deliverData(id uint32, data []byte) {
	m.mu.RLock()
	conn, ok := m.conns[id]
	m.mu.RUnlock()

	if ok && !conn.closed {
		// Copy data since the buffer may be reused
		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)

		select {
		case conn.readBuf <- dataCopy:
		default:
			// Buffer full, drop data
			proxyLogger.Warn("Read buffer full for connection", "connID", id)
		}
	}
}

func (m *connectionManager) closeConn(id uint32) {
	m.mu.RLock()
	conn, ok := m.conns[id]
	m.mu.RUnlock()

	if ok {
		conn.Close()
	}
}

func newVirtualConn(id uint32, server *SOCKS5Server) *virtualConn {
	conn := &virtualConn{
		id:      id,
		server:  server,
		readBuf: make(chan []byte, 256),
	}
	connMgr.register(conn)
	return conn
}

func (c *virtualConn) Read(b []byte) (int, error) {
	if c.closed {
		return 0, io.EOF
	}

	select {
	case data, ok := <-c.readBuf:
		if !ok {
			return 0, io.EOF
		}
		n := copy(b, data)
		return n, nil
	case <-time.After(time.Minute):
		return 0, fmt.Errorf("read timeout")
	}
}

func (c *virtualConn) Write(b []byte) (int, error) {
	if c.closed {
		return 0, io.ErrClosedPipe
	}

	msg := protocol.NewDataMessage(c.id, b)
	if err := c.server.sendToAgent(msg); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *virtualConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeMu.Lock()
		c.closed = true
		close(c.readBuf)
		c.closeMu.Unlock()

		connMgr.unregister(c.id)

		msg := protocol.NewCloseMessage(c.id)
		c.server.sendToAgent(msg)
	})
	return nil
}

func (c *virtualConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
}

func (c *virtualConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 0}
}

func (c *virtualConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *virtualConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *virtualConn) SetWriteDeadline(t time.Time) error {
	return nil
}
