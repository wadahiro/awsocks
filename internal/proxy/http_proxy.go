package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/mux"
	"github.com/wadahiro/awsocks/internal/routing"
)

var httpProxyLogger = log.For(log.ComponentProxy)

// HTTPProxyServer provides an HTTP CONNECT proxy that shares the same
// routing and dial logic as the SOCKS5 proxy via ProxyDialer.
type HTTPProxyServer struct {
	cfg        *Config
	proxyDial  *ProxyDialer
	listener   net.Listener
	listenerMu sync.Mutex
}

// NewHTTPProxyServer creates a new HTTP CONNECT proxy server
func NewHTTPProxyServer(cfg *Config, router routing.Router, agentMux *mux.AgentMux) *HTTPProxyServer {
	return &HTTPProxyServer{
		cfg:       cfg,
		proxyDial: NewProxyDialer(router, agentMux),
	}
}

// Dialer returns the underlying ProxyDialer for shared access
func (s *HTTPProxyServer) Dialer() *ProxyDialer {
	return s.proxyDial
}

// Start starts the HTTP CONNECT proxy server
func (s *HTTPProxyServer) Start() error {
	listener, err := net.Listen("tcp", s.cfg.HTTPListenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.cfg.HTTPListenAddr, err)
	}

	s.listenerMu.Lock()
	s.listener = listener
	s.listenerMu.Unlock()

	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(conn)
	}
}

// Stop stops the HTTP CONNECT proxy server
func (s *HTTPProxyServer) Stop() {
	s.listenerMu.Lock()
	if s.listener != nil {
		s.listener.Close()
	}
	s.listenerMu.Unlock()
}

// handleConn handles a single client connection
func (s *HTTPProxyServer) handleConn(conn net.Conn) {
	defer conn.Close()

	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		httpProxyLogger.Debug("Failed to read HTTP request", "error", err)
		return
	}

	if req.Method != http.MethodConnect {
		resp := &http.Response{
			StatusCode: http.StatusMethodNotAllowed,
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
		}
		resp.Header.Set("Content-Length", "0")
		resp.Write(conn)
		return
	}

	addr := req.Host

	httpProxyLogger.Info("HTTP CONNECT", "address", addr)

	// Dial the target using shared ProxyDialer
	targetConn, err := s.proxyDial.Dial(req.Context(), "tcp", addr)
	if err != nil {
		httpProxyLogger.Warn("HTTP CONNECT dial failed", "address", addr, "error", err)
		resp := &http.Response{
			StatusCode: http.StatusBadGateway,
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
		}
		resp.Header.Set("Content-Length", "0")
		resp.Write(conn)
		return
	}
	defer targetConn.Close()

	// Send 200 Connection Established
	conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Bidirectional copy
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(targetConn, br)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(conn, targetConn)
		done <- struct{}{}
	}()

	// Wait for either direction to finish
	<-done
}
