package proxy

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/routing"
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

// mockLazyInitForDirect implements LazyInitializer for direct mode testing
type mockLazyInitForDirect struct {
	initDone chan struct{}
	initErr  error
}

func newMockLazyInitForDirect() *mockLazyInitForDirect {
	return &mockLazyInitForDirect{
		initDone: make(chan struct{}),
	}
}

func (m *mockLazyInitForDirect) EnsureInitialized(ctx context.Context) error {
	return m.initErr
}

func (m *mockLazyInitForDirect) InitDone() <-chan struct{} {
	return m.initDone
}

func (m *mockLazyInitForDirect) InitError() error {
	return m.initErr
}

func (m *mockLazyInitForDirect) completeInit() {
	close(m.initDone)
}

func (m *mockLazyInitForDirect) failInit(err error) {
	m.initErr = err
	close(m.initDone)
}

func TestDirectDialer_RouteProxy_WaitsForInit(t *testing.T) {
	// Start a dummy TCP server as backend target
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

	cfg := &Config{ListenAddr: "127.0.0.1:0"}
	router := &mockRouter{route: routing.RouteProxy}
	server := NewDirectSOCKS5Server(cfg, nil, router)

	mock := newMockLazyInitForDirect()
	server.SetLazyInitializer(mock)

	// Set backend dialer that connects to dummy server
	server.SetBackendDialer(&mockBackendForTest{
		dialFunc: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, dummyAddr)
		},
	})

	dialer := &directDialer{cfg: cfg, server: server, router: router}

	// Dial should block until init completes
	type dialResult struct {
		conn net.Conn
		err  error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := dialer.Dial(context.Background(), "tcp", "internal.example.com:443")
		resultCh <- dialResult{conn: conn, err: err}
	}()

	// Verify it's still waiting
	select {
	case <-resultCh:
		t.Fatal("Dial should not return before init completes")
	case <-time.After(100 * time.Millisecond):
		// Expected: still waiting
	}

	// Complete initialization
	mock.completeInit()

	// Now it should complete
	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		assert.NotNil(t, result.conn)
		if result.conn != nil {
			result.conn.Close()
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dial did not return after init completed")
	}
}

func TestDirectDialer_RouteProxy_InitFailure(t *testing.T) {
	cfg := &Config{ListenAddr: "127.0.0.1:0"}
	router := &mockRouter{route: routing.RouteProxy}
	server := NewDirectSOCKS5Server(cfg, nil, router)

	mock := newMockLazyInitForDirect()
	server.SetLazyInitializer(mock)

	dialer := &directDialer{cfg: cfg, server: server, router: router}

	type dialResult struct {
		conn net.Conn
		err  error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := dialer.Dial(context.Background(), "tcp", "internal.example.com:443")
		resultCh <- dialResult{conn: conn, err: err}
	}()

	// Wait for Dial goroutine to reach the select{} wait
	time.Sleep(100 * time.Millisecond)

	// Fail initialization
	mock.failInit(fmt.Errorf("AWS credentials expired"))

	select {
	case result := <-resultCh:
		assert.Nil(t, result.conn)
		require.Error(t, result.err)
		assert.Contains(t, result.err.Error(), "initialization failed")
		assert.Contains(t, result.err.Error(), "AWS credentials expired")
	case <-time.After(2 * time.Second):
		t.Fatal("Dial did not return after init failed")
	}
}

func TestDirectDialer_RouteDirect_NoWait(t *testing.T) {
	// Start a dummy TCP server for direct connection
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

	cfg := &Config{ListenAddr: "127.0.0.1:0"}
	router := &mockRouter{route: routing.RouteDirect}
	server := NewDirectSOCKS5Server(cfg, nil, router)

	mock := newMockLazyInitForDirect()
	server.SetLazyInitializer(mock)
	// Note: initDone is NOT closed, so init is still "in progress"

	dialer := &directDialer{cfg: cfg, server: server, router: router}

	// RouteDirect should return immediately, not wait for init
	conn, err := dialer.Dial(context.Background(), "tcp", dummyAddr)
	require.NoError(t, err)
	assert.NotNil(t, conn)
	if conn != nil {
		conn.Close()
	}
}

func TestDirectDialer_RouteProxy_ContextCancel(t *testing.T) {
	cfg := &Config{ListenAddr: "127.0.0.1:0"}
	router := &mockRouter{route: routing.RouteProxy}
	server := NewDirectSOCKS5Server(cfg, nil, router)

	mock := newMockLazyInitForDirect()
	server.SetLazyInitializer(mock)

	dialer := &directDialer{cfg: cfg, server: server, router: router}

	ctx, cancel := context.WithCancel(context.Background())

	type dialResult struct {
		conn net.Conn
		err  error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := dialer.Dial(ctx, "tcp", "internal.example.com:443")
		resultCh <- dialResult{conn: conn, err: err}
	}()

	// Cancel context while waiting for init
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case result := <-resultCh:
		assert.Nil(t, result.conn)
		require.Error(t, result.err)
		assert.ErrorIs(t, result.err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Dial did not return after context cancellation")
	}
}
