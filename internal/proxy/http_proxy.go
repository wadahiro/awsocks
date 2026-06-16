package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/mux"
	"github.com/wadahiro/awsocks/internal/routing"
)

var httpProxyLogger = log.For(log.ComponentProxy)

// HTTPProxyServer provides an HTTP forward/CONNECT proxy that shares the same
// routing and dial logic as the SOCKS5 proxy via ProxyDialer.
type HTTPProxyServer struct {
	cfg        *Config
	proxyDial  *ProxyDialer
	listener   net.Listener
	listenerMu sync.Mutex
}

// NewHTTPProxyServer creates a new HTTP proxy server
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

// Start starts the HTTP proxy server
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

// Stop stops the HTTP proxy server
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

	if req.Method == http.MethodConnect {
		s.handleCONNECT(conn, br, req)
	} else {
		s.handleForward(conn, req)
	}
}

// handleCONNECT handles HTTPS tunneling via CONNECT method
func (s *HTTPProxyServer) handleCONNECT(conn net.Conn, br *bufio.Reader, req *http.Request) {
	addr := req.Host

	httpProxyLogger.Info("HTTP CONNECT", "address", addr)

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

	<-done
}

// handleForward handles plain HTTP requests by forwarding them to the target
func (s *HTTPProxyServer) handleForward(conn net.Conn, req *http.Request) {
	// Determine target address from the absolute URL
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	if host == "" {
		writeHTTPError(conn, http.StatusBadRequest)
		return
	}

	// Add default port if missing
	addr := host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		port := "80"
		if req.URL.Scheme == "https" {
			port = "443"
		}
		addr = net.JoinHostPort(host, port)
	}

	httpProxyLogger.Info("HTTP forward", "method", req.Method, "address", addr, "path", req.URL.Path)

	targetConn, err := s.proxyDial.Dial(req.Context(), "tcp", addr)
	if err != nil {
		httpProxyLogger.Warn("HTTP forward dial failed", "address", addr, "error", err)
		writeHTTPError(conn, http.StatusBadGateway)
		return
	}
	defer targetConn.Close()

	// Rewrite the request: convert absolute URL to relative path for the target
	req.URL.Scheme = ""
	req.URL.Host = ""
	req.RequestURI = req.URL.RequestURI()

	// Remove hop-by-hop headers
	removeHopByHopHeaders(req.Header)

	// Forward the request
	if err := req.Write(targetConn); err != nil {
		httpProxyLogger.Warn("HTTP forward write failed", "address", addr, "error", err)
		writeHTTPError(conn, http.StatusBadGateway)
		return
	}

	// Copy the response back
	io.Copy(conn, targetConn)
}

// removeHopByHopHeaders removes headers that should not be forwarded
func removeHopByHopHeaders(h http.Header) {
	// Standard hop-by-hop headers
	hopByHop := []string{
		"Proxy-Connection",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	}

	// Also remove headers listed in Connection header
	for _, v := range h.Values("Connection") {
		for _, key := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(key))
		}
	}
	h.Del("Connection")

	for _, key := range hopByHop {
		h.Del(key)
	}
}

// writeHTTPError writes an HTTP error response
func writeHTTPError(conn net.Conn, statusCode int) {
	resp := &http.Response{
		StatusCode: statusCode,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
	}
	resp.Header.Set("Content-Length", "0")
	resp.Write(conn)
}
