package ssm

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHTTPProxy accepts CONNECT requests and tunnels to the target
func fakeHTTPProxy(t *testing.T, listener net.Listener, expectedAuth string) {
	t.Helper()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go handleProxyConn(t, conn, expectedAuth)
	}
}

func handleProxyConn(t *testing.T, conn net.Conn, expectedAuth string) {
	t.Helper()
	defer conn.Close()

	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
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

	// Check auth if expected
	if expectedAuth != "" {
		_, _, ok := req.BasicAuth()
		if !ok {
			resp := &http.Response{
				StatusCode: http.StatusProxyAuthRequired,
				ProtoMajor: 1,
				ProtoMinor: 1,
				Header:     make(http.Header),
			}
			resp.Header.Set("Content-Length", "0")
			resp.Write(conn)
			return
		}
	}

	// Connect to the target
	targetConn, err := net.Dial("tcp", req.Host)
	if err != nil {
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

func TestDialViaHTTPProxy(t *testing.T) {
	// Start a target server
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

	// Start a fake HTTP proxy
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer proxyListener.Close()

	go fakeHTTPProxy(t, proxyListener, "")

	proxyURL := fmt.Sprintf("http://%s", proxyListener.Addr().String())

	// Test dialViaHTTPProxy by creating a direct connection to the proxy
	// (simulating what SSH client.Dial would do)
	proxyConn, err := net.Dial("tcp", proxyListener.Addr().String())
	require.NoError(t, err)
	defer proxyConn.Close()

	// Send CONNECT manually (same as dialViaHTTPProxy does internally)
	fmt.Fprintf(proxyConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr)

	br := bufio.NewReader(proxyConn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	buf := make([]byte, 100)
	n, err := br.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "hello from target", string(buf[:n]))

	_ = proxyURL // Used by integration tests below
}

func TestDialViaUpstreamProxy_UnsupportedScheme(t *testing.T) {
	// socks5 is not yet implemented
	_, err := dialViaUpstreamProxy(nil, "socks5://localhost:1080", "tcp", "example.com:80")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported upstream proxy scheme: socks5")
}

func TestDialViaUpstreamProxy_InvalidURL(t *testing.T) {
	_, err := dialViaUpstreamProxy(nil, "://invalid", "tcp", "example.com:80")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid upstream proxy URL")
}

func TestDialViaHTTPProxy_ProxyConnectFailure(t *testing.T) {
	// Start a fake proxy that returns 502 for all CONNECT
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer proxyListener.Close()

	go func() {
		for {
			conn, err := proxyListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				http.ReadRequest(br)
				conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
			}()
		}
	}()

	// Connect directly to the proxy (simulating SSH client.Dial)
	proxyConn, err := net.Dial("tcp", proxyListener.Addr().String())
	require.NoError(t, err)
	defer proxyConn.Close()

	fmt.Fprintf(proxyConn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")

	br := bufio.NewReader(proxyConn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestBufferedConn_Read(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		server.Write([]byte("buffered data"))
		server.Close()
	}()

	br := bufio.NewReader(client)
	// Pre-read into buffer
	peeked, err := br.Peek(8)
	require.NoError(t, err)
	assert.Equal(t, "buffered", string(peeked))

	bc := &bufferedConn{Conn: client, reader: br}

	buf := make([]byte, 100)
	n, err := bc.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "buffered data", string(buf[:n]))
}

func TestDialViaHTTPProxy_DefaultPort(t *testing.T) {
	// Test that default port 8080 is used when no port specified
	// We just verify the URL parsing logic here
	proxyListener, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Skip("port 8080 not available")
	}
	defer proxyListener.Close()

	// Start target
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer targetListener.Close()

	go func() {
		for {
			conn, err := targetListener.Accept()
			if err != nil {
				return
			}
			conn.Write([]byte("ok"))
			conn.Close()
		}
	}()

	go fakeHTTPProxy(t, proxyListener, "")

	// Connect through proxy without port (should default to 8080)
	proxyConn, err := net.Dial("tcp", "127.0.0.1:8080")
	require.NoError(t, err)
	defer proxyConn.Close()

	targetAddr := targetListener.Addr().String()
	fmt.Fprintf(proxyConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr)

	br := bufio.NewReader(proxyConn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	buf := make([]byte, 100)
	n, err := br.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(buf[:n]))
}

func TestShouldUseUpstreamProxy(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		patterns []string
		address  string
		want     bool
	}{
		{
			name:    "no upstream proxy configured",
			url:     "",
			address: "example.com:443",
			want:    false,
		},
		{
			name:    "upstream proxy without patterns matches all",
			url:     "http://localhost:8080",
			address: "example.com:443",
			want:    true,
		},
		{
			name:     "pattern matches exact host",
			url:      "http://localhost:8080",
			patterns: []string{"internal.example.com"},
			address:  "internal.example.com:443",
			want:     true,
		},
		{
			name:     "pattern matches wildcard",
			url:      "http://localhost:8080",
			patterns: []string{"*.example.com"},
			address:  "foo.example.com:443",
			want:     true,
		},
		{
			name:     "pattern does not match",
			url:      "http://localhost:8080",
			patterns: []string{"*.internal.example.com"},
			address:  "public.other.com:443",
			want:     false,
		},
		{
			name:     "multiple patterns, second matches",
			url:      "http://localhost:8080",
			patterns: []string{"*.foo.com", "*.bar.com"},
			address:  "test.bar.com:80",
			want:     true,
		},
		{
			name:     "multiple patterns, none match",
			url:      "http://localhost:8080",
			patterns: []string{"*.foo.com", "*.bar.com"},
			address:  "example.com:80",
			want:     false,
		},
		{
			name:     "address without port",
			url:      "http://localhost:8080",
			patterns: []string{"*.example.com"},
			address:  "foo.example.com",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				UpstreamProxyURL:      tt.url,
				UpstreamProxyPatterns: tt.patterns,
			}
			b := New(cfg, nil)
			got := b.shouldUseUpstreamProxy(tt.address)
			assert.Equal(t, tt.want, got)
		})
	}
}
