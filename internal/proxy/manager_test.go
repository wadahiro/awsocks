package proxy

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirectSOCKS5Server_StopClosesListener(t *testing.T) {
	cfg := &Config{
		ListenAddr: "127.0.0.1:0",
	}

	server := NewDirectSOCKS5Server(cfg, nil, nil)

	// Start server in goroutine
	startErr := make(chan error, 1)
	go func() {
		startErr <- server.Start()
	}()

	// Wait for server to start
	time.Sleep(100 * time.Millisecond)

	// Verify listener is set
	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener, "listener should be set after Start()")

	// Get the actual listen address
	addr := listener.Addr().String()

	// Verify we can connect
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	require.NoError(t, err, "should be able to connect to server")
	conn.Close()

	// Stop the server
	server.Stop()

	// Wait for Start() to return
	select {
	case err := <-startErr:
		// Start() should return with an error (listener closed)
		assert.Error(t, err, "Start() should return error when listener is closed")
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return after Stop() was called")
	}

	// Verify we can no longer connect
	_, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
	assert.Error(t, err, "should not be able to connect after Stop()")
}

func TestDirectSOCKS5Server_StopBeforeStart(t *testing.T) {
	cfg := &Config{
		ListenAddr: "127.0.0.1:0",
	}

	server := NewDirectSOCKS5Server(cfg, nil, nil)

	// Stop before Start should not panic
	assert.NotPanics(t, func() {
		server.Stop()
	})
}

// mockBackendForTest implements a minimal backend for testing
type mockBackendForTest struct {
	dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)
	closed   bool
}

func (m *mockBackendForTest) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if m.dialFunc != nil {
		return m.dialFunc(ctx, network, addr)
	}
	return nil, nil
}

func TestDirectSOCKS5Server_UsesBackendWhenProvided(t *testing.T) {
	// Start a dummy TCP server to act as the target
	dummyListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer dummyListener.Close()

	dummyAddr := dummyListener.Addr().String()

	// Accept connections on dummy server
	go func() {
		for {
			conn, err := dummyListener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	dialCalled := false
	var dialedAddr string
	mockBe := &mockBackendForTest{
		dialFunc: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialCalled = true
			dialedAddr = addr
			// Connect to the dummy server
			return net.Dial(network, dummyAddr)
		},
	}

	cfg := &Config{
		ListenAddr: "127.0.0.1:0",
	}

	server := NewDirectSOCKS5Server(cfg, nil, nil)
	server.SetBackendDialer(mockBe)

	// Start server in goroutine
	go func() {
		server.Start()
	}()

	// Wait for server to start
	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener)

	addr := listener.Addr().String()

	// Connect through SOCKS5
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	require.NoError(t, err)

	// Send SOCKS5 handshake (version 5, 1 auth method, no auth)
	_, err = conn.Write([]byte{0x05, 0x01, 0x00})
	require.NoError(t, err)

	// Read server response
	buf := make([]byte, 2)
	_, err = conn.Read(buf)
	require.NoError(t, err)

	// Send connect request to example.com:80
	// Version, Connect, Reserved, Domain, len, domain, port
	request := []byte{0x05, 0x01, 0x00, 0x03, 0x0b}
	request = append(request, []byte("example.com")...)
	request = append(request, 0x00, 0x50) // Port 80
	_, err = conn.Write(request)
	require.NoError(t, err)

	// Read connect response
	response := make([]byte, 10)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	conn.Read(response)

	conn.Close()
	server.Stop()

	// Verify backend dial was called with hostname (not IP)
	// This is critical for remote DNS resolution via SSH dynamic port forwarding
	assert.True(t, dialCalled, "backend Dial should be called when backend is provided")
	assert.Equal(t, "example.com:80", dialedAddr, "backend should receive hostname, not resolved IP")
}

func TestDirectSOCKS5Server_FallsBackToDirectWhenNoBackend(t *testing.T) {
	cfg := &Config{
		ListenAddr: "127.0.0.1:0",
	}

	// No backend provided (nil)
	server := NewDirectSOCKS5Server(cfg, nil, nil)

	// Start server in goroutine
	go func() {
		server.Start()
	}()

	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener)

	addr := listener.Addr().String()

	// Connect through SOCKS5
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	require.NoError(t, err)

	// Send SOCKS5 handshake
	_, err = conn.Write([]byte{0x05, 0x01, 0x00})
	require.NoError(t, err)

	// Read server response (should accept no-auth)
	buf := make([]byte, 2)
	_, err = conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, byte(0x05), buf[0], "should be SOCKS5")
	assert.Equal(t, byte(0x00), buf[1], "should accept no-auth")

	conn.Close()
	server.Stop()
}
