package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/routing"
)

func TestHTTPProxyServer_CONNECT(t *testing.T) {
	// Start a dummy target server
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer targetListener.Close()

	go func() {
		for {
			conn, err := targetListener.Accept()
			if err != nil {
				return
			}
			conn.Write([]byte("hello from target"))
			conn.Close()
		}
	}()
	targetAddr := targetListener.Addr().String()

	// Start HTTP proxy with direct routing
	router := &mockRouter{route: routing.RouteDirect}
	cfg := &Config{
		HTTPListenAddr: "127.0.0.1:0",
	}
	server := NewHTTPProxyServer(cfg, router, nil)

	startErr := make(chan error, 1)
	go func() {
		startErr <- server.Start()
	}()
	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener)
	proxyAddr := listener.Addr().String()

	// Send CONNECT request
	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	require.NoError(t, err)
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr)

	// Read response
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// After CONNECT, we should receive target data
	buf := make([]byte, 100)
	n, err := br.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "hello from target", string(buf[:n]))

	server.Stop()
}

func TestHTTPProxyServer_ForwardGET(t *testing.T) {
	// Start a target HTTP server
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer targetListener.Close()

	go func() {
		for {
			conn, err := targetListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				// Verify the request is relative (not absolute URL)
				assert.Equal(t, "/api/data", req.URL.Path)
				assert.Equal(t, "", req.URL.Host)
				// Verify hop-by-hop headers are removed
				assert.Empty(t, req.Header.Get("Proxy-Connection"))

				body := `{"ok": true}`
				fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
			}()
		}
	}()
	targetAddr := targetListener.Addr().String()

	router := &mockRouter{route: routing.RouteDirect}
	cfg := &Config{
		HTTPListenAddr: "127.0.0.1:0",
	}
	server := NewHTTPProxyServer(cfg, router, nil)

	go server.Start()
	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener)
	proxyAddr := listener.Addr().String()

	// Send GET with absolute URL (as HTTP proxy clients do)
	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	require.NoError(t, err)
	defer conn.Close()

	fmt.Fprintf(conn, "GET http://%s/api/data HTTP/1.1\r\nHost: %s\r\nProxy-Connection: Keep-Alive\r\n\r\n", targetAddr, targetAddr)

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, `{"ok": true}`, string(body))

	server.Stop()
}

func TestHTTPProxyServer_ForwardGET_ClientDisconnectClosesTarget(t *testing.T) {
	// Target accepts the request but never responds, simulating an upstream
	// that is hung/slow, and reports whether/when its connection was closed.
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer targetListener.Close()

	targetClosed := make(chan struct{})
	go func() {
		conn, err := targetListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		if _, err := http.ReadRequest(br); err != nil {
			return
		}
		// Never write a response. Detect the proxy closing its side by
		// observing a read error/EOF on our end.
		buf := make([]byte, 1)
		conn.Read(buf)
		close(targetClosed)
	}()
	targetAddr := targetListener.Addr().String()

	router := &mockRouter{route: routing.RouteDirect}
	cfg := &Config{
		HTTPListenAddr: "127.0.0.1:0",
	}
	server := NewHTTPProxyServer(cfg, router, nil)

	go server.Start()
	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener)
	proxyAddr := listener.Addr().String()

	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	require.NoError(t, err)

	fmt.Fprintf(conn, "GET http://%s/api/data HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr)

	// The client aborts (e.g. ESC) before the target ever answers.
	conn.Close()

	select {
	case <-targetClosed:
		// expected: client disconnect propagates to the target connection
	case <-time.After(2 * time.Second):
		t.Fatal("target connection was not closed after client disconnected")
	}

	server.Stop()
}

func TestHTTPProxyServer_CONNECT_BadAddress(t *testing.T) {
	router := &mockRouter{route: routing.RouteDirect}
	cfg := &Config{
		HTTPListenAddr: "127.0.0.1:0",
	}
	server := NewHTTPProxyServer(cfg, router, nil)

	go server.Start()
	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener)
	proxyAddr := listener.Addr().String()

	// Send CONNECT with unreachable address
	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	require.NoError(t, err)
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT 127.0.0.1:1 HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)

	server.Stop()
}

func TestHTTPProxyServer_CONNECT_SharesProxyDialer(t *testing.T) {
	// Verify that HTTP proxy uses ProxyDialer and routing works
	var dialedAddr string
	dummyListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer dummyListener.Close()
	dummyAddr := dummyListener.Addr().String()
	go func() {
		for {
			conn, err := dummyListener.Accept()
			if err != nil {
				return
			}
			conn.Write([]byte("proxied"))
			conn.Close()
		}
	}()

	router := &mockRouter{route: routing.RouteProxy}
	cfg := &Config{
		HTTPListenAddr: "127.0.0.1:0",
	}
	server := NewHTTPProxyServer(cfg, router, nil)
	server.Dialer().SetBackendDialer(&mockBackendForTest{
		dialFunc: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialedAddr = addr
			return net.Dial(network, dummyAddr)
		},
	})

	go server.Start()
	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener)
	proxyAddr := listener.Addr().String()

	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	require.NoError(t, err)
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT internal.example.com:443 HTTP/1.1\r\nHost: internal.example.com:443\r\n\r\n")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	buf := make([]byte, 100)
	n, err := br.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "proxied", string(buf[:n]))
	assert.Equal(t, "internal.example.com:443", dialedAddr)

	server.Stop()
}

func TestHTTPProxyServer_CONNECT_LazyInit(t *testing.T) {
	dummyListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer dummyListener.Close()
	dummyAddr := dummyListener.Addr().String()
	go func() {
		for {
			conn, err := dummyListener.Accept()
			if err != nil {
				return
			}
			conn.Write([]byte("ok"))
			conn.Close()
		}
	}()

	router := &mockRouter{route: routing.RouteProxy}
	cfg := &Config{
		HTTPListenAddr: "127.0.0.1:0",
	}
	server := NewHTTPProxyServer(cfg, router, nil)

	mock := newMockLazyInit()
	server.Dialer().SetLazyInitializer(mock)
	server.Dialer().SetBackendDialer(&mockBackendForTest{
		dialFunc: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, dummyAddr)
		},
	})

	go server.Start()
	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener)
	proxyAddr := listener.Addr().String()

	// Connect and send CONNECT
	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	require.NoError(t, err)
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")

	// Response should be blocked waiting for init
	responseCh := make(chan *http.Response, 1)
	go func() {
		br := bufio.NewReader(conn)
		resp, _ := http.ReadResponse(br, nil)
		responseCh <- resp
	}()

	select {
	case <-responseCh:
		t.Fatal("should not get response before init completes")
	case <-time.After(200 * time.Millisecond):
	}

	// Complete init
	mock.completeInit()

	select {
	case resp := <-responseCh:
		require.NotNil(t, resp)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	case <-time.After(2 * time.Second):
		t.Fatal("did not get response after init completed")
	}

	server.Stop()
}

func TestHTTPProxyServer_Stop(t *testing.T) {
	cfg := &Config{
		HTTPListenAddr: "127.0.0.1:0",
	}
	server := NewHTTPProxyServer(cfg, nil, nil)

	go server.Start()
	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener)
	addr := listener.Addr().String()

	server.Stop()

	_, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	assert.Error(t, err)
}

func TestHTTPProxyServer_StopBeforeStart(t *testing.T) {
	cfg := &Config{
		HTTPListenAddr: "127.0.0.1:0",
	}
	server := NewHTTPProxyServer(cfg, nil, nil)

	assert.NotPanics(t, func() {
		server.Stop()
	})
}

func TestHTTPProxyServer_CONNECT_BidirectionalData(t *testing.T) {
	// Test that data flows in both directions after CONNECT
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer targetListener.Close()

	go func() {
		conn, err := targetListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Echo back whatever we receive
		io.Copy(conn, conn)
	}()
	targetAddr := targetListener.Addr().String()

	router := &mockRouter{route: routing.RouteDirect}
	cfg := &Config{
		HTTPListenAddr: "127.0.0.1:0",
	}
	server := NewHTTPProxyServer(cfg, router, nil)

	go server.Start()
	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	proxyAddr := listener.Addr().String()

	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	require.NoError(t, err)
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr)

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Send data through tunnel
	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)

	buf := make([]byte, 100)
	n, err := br.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf[:n]))

	server.Stop()
}

func TestHTTPProxyServer_CONNECT_IdleTrackerTouch(t *testing.T) {
	dummyListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer dummyListener.Close()
	dummyAddr := dummyListener.Addr().String()
	go func() {
		for {
			conn, err := dummyListener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	router := &mockRouter{route: routing.RouteProxy}
	cfg := &Config{
		HTTPListenAddr: "127.0.0.1:0",
	}
	server := NewHTTPProxyServer(cfg, router, nil)
	server.Dialer().SetBackendDialer(&mockBackendForTest{
		dialFunc: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, dummyAddr)
		},
	})

	go server.Start()
	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener)
	proxyAddr := listener.Addr().String()

	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	require.NoError(t, err)
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	server.Stop()
}
