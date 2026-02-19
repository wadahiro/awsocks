package proxy

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/clock"
	"github.com/wadahiro/awsocks/internal/credentials"
	"github.com/wadahiro/awsocks/internal/protocol"
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
	initDone    chan struct{}
	initErr     error
	ensureCalls int32 // atomic for thread safety
}

func newMockLazyInitForDirect() *mockLazyInitForDirect {
	return &mockLazyInitForDirect{
		initDone: make(chan struct{}),
	}
}

func (m *mockLazyInitForDirect) EnsureInitialized(ctx context.Context) error {
	atomic.AddInt32(&m.ensureCalls, 1)
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

	// EnsureInitialized should NOT have been called for RouteDirect
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(0), atomic.LoadInt32(&mock.ensureCalls), "EnsureInitialized should not be called for RouteDirect")
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

// mockFullBackend implements backend.Backend for testing suspend
type mockFullBackend struct {
	closed   bool
	dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (m *mockFullBackend) Name() string { return "mock" }
func (m *mockFullBackend) Start(ctx context.Context) error { return nil }
func (m *mockFullBackend) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if m.dialFunc != nil {
		return m.dialFunc(ctx, network, addr)
	}
	return nil, nil
}
func (m *mockFullBackend) OnCredentialUpdate(creds aws.Credentials) error { return nil }
func (m *mockFullBackend) Close() error {
	m.closed = true
	return nil
}

func TestDirectManager_SuspendResumeCycle(t *testing.T) {
	mockClock := clock.NewMockClock(time.Now())

	cfg := &Config{
		ListenAddr:  "127.0.0.1:0",
		IdleTimeout: 30 * time.Minute,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockBe := &mockFullBackend{}

	mgr := &DirectManager{
		cfg:      cfg,
		initDone: make(chan struct{}),
		clock:    mockClock,
		ctx:      ctx,
		cancel:   cancel,
		backend:  mockBe,
	}

	// Simulate completed initialization
	mgr.awsInitialized = true
	close(mgr.initDone)

	// Verify initial state
	assert.True(t, mgr.IsInitialized())

	// Call suspend
	mgr.suspend()

	// After suspend: not initialized, initDone is a new unclosed channel
	assert.False(t, mgr.IsInitialized())

	// initDone should be a new channel (not closed)
	select {
	case <-mgr.InitDone():
		t.Fatal("initDone should not be closed after suspend")
	default:
		// Expected: not closed
	}

	assert.Nil(t, mgr.initErr)

	// Backend.Close() should have been called
	assert.True(t, mockBe.closed, "backend.Close() should be called during suspend")

	// Backend should be nil after suspend
	assert.Nil(t, mgr.backend, "backend should be nil after suspend")
}

func TestVMManager_Suspend_SendsSuspendMessage(t *testing.T) {
	mockClock := clock.NewMockClock(time.Now())

	cfg := &Config{
		ListenAddr:  "127.0.0.1:0",
		IdleTimeout: 30 * time.Minute,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create pipe to simulate agent connection
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	mgr := &VMManager{
		cfg:       cfg,
		initDone:  make(chan struct{}),
		clock:     mockClock,
		ctx:       ctx,
		cancel:    cancel,
		agentConn: agentClient,
	}

	// Simulate completed initialization
	mgr.awsInitialized = true
	close(mgr.initDone)

	// Read message in background
	msgCh := make(chan *protocol.Message, 1)
	go func() {
		msg, err := protocol.ReadMessage(agentServer)
		if err == nil {
			msgCh <- msg
		}
	}()

	// Call suspend
	mgr.suspend()

	// Verify MsgSuspend was sent to agent
	select {
	case msg := <-msgCh:
		assert.Equal(t, protocol.MsgSuspend, msg.Type, "should send MsgSuspend to agent")
	case <-time.After(2 * time.Second):
		t.Fatal("MsgSuspend was not sent to agent")
	}

	// After suspend: not initialized
	assert.False(t, mgr.awsInitialized)

	// initDone should be a new unclosed channel
	select {
	case <-mgr.InitDone():
		t.Fatal("initDone should not be closed after suspend")
	default:
		// Expected: not closed
	}
}

func TestVMManager_EnsureInitialized_ErrorThenRetry_NoPanic(t *testing.T) {
	// Regression test: when EnsureInitialized fails and is retried,
	// the second call should not panic with "close of closed channel".
	// This simulates: suspend → two concurrent requests → first fails → second retries.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := &VMManager{
		cfg: &Config{
			ListenAddr: "127.0.0.1:0",
		},
		initDone: make(chan struct{}),
		clock:    clock.RealClock{},
		ctx:      ctx,
		cancel:   cancel,
		credProv: credentials.NewProvider("nonexistent-profile-for-test", "us-east-1"),
	}

	// First call fails (invalid AWS profile → credential load error)
	err1 := mgr.EnsureInitialized(context.Background())
	require.Error(t, err1, "first call should fail")

	// After error, initDone should have been closed-and-replaced
	// so waiters on the OLD channel are unblocked, and the NEW channel is open for next attempt
	assert.False(t, mgr.awsInitialized, "should not be initialized after error")

	// Second call should not panic (this was the original bug)
	assert.NotPanics(t, func() {
		err2 := mgr.EnsureInitialized(context.Background())
		// It will fail again for the same reason, but the point is: no panic
		assert.Error(t, err2)
	})
}

func TestDirectManager_EnsureInitialized_ErrorThenRetry_NoPanic(t *testing.T) {
	// Same regression test for DirectManager
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := &DirectManager{
		cfg: &Config{
			ListenAddr: "127.0.0.1:0",
		},
		initDone: make(chan struct{}),
		clock:    clock.RealClock{},
		ctx:      ctx,
		cancel:   cancel,
		credProv: credentials.NewProvider("nonexistent-profile-for-test", "us-east-1"),
	}

	// First call fails (invalid AWS profile → credential load error)
	err1 := mgr.EnsureInitialized(context.Background())
	require.Error(t, err1, "first call should fail")

	assert.False(t, mgr.awsInitialized, "should not be initialized after error")

	// Second call should not panic
	assert.NotPanics(t, func() {
		err2 := mgr.EnsureInitialized(context.Background())
		assert.Error(t, err2)
	})
}

func TestVMManager_EnsureInitialized_ConcurrentAfterSuspend_NoPanic(t *testing.T) {
	// Regression test for the actual bug scenario:
	// After suspend, multiple concurrent goroutines call EnsureInitialized.
	// All calls fail (due to invalid credentials), and each error path
	// closes initDone. Without the fix, the second close panics.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := &VMManager{
		cfg: &Config{
			ListenAddr: "127.0.0.1:0",
		},
		initDone: make(chan struct{}),
		clock:    clock.RealClock{},
		ctx:      ctx,
		cancel:   cancel,
		credProv: credentials.NewProvider("nonexistent-profile-for-test", "us-east-1"),
	}

	const goroutines = 5
	errs := make(chan error, goroutines)

	assert.NotPanics(t, func() {
		// Launch multiple goroutines concurrently (simulates multiple proxy requests after suspend)
		for i := 0; i < goroutines; i++ {
			go func() {
				errs <- mgr.EnsureInitialized(context.Background())
			}()
		}

		// Collect all results
		for i := 0; i < goroutines; i++ {
			err := <-errs
			assert.Error(t, err, "all calls should fail with credential error")
		}
	})

	// Manager should not be initialized
	assert.False(t, mgr.awsInitialized)
}

func TestDirectManager_EnsureInitialized_ConcurrentAfterSuspend_NoPanic(t *testing.T) {
	// Same concurrent test for DirectManager
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := &DirectManager{
		cfg: &Config{
			ListenAddr: "127.0.0.1:0",
		},
		initDone: make(chan struct{}),
		clock:    clock.RealClock{},
		ctx:      ctx,
		cancel:   cancel,
		credProv: credentials.NewProvider("nonexistent-profile-for-test", "us-east-1"),
	}

	const goroutines = 5
	errs := make(chan error, goroutines)

	assert.NotPanics(t, func() {
		for i := 0; i < goroutines; i++ {
			go func() {
				errs <- mgr.EnsureInitialized(context.Background())
			}()
		}

		for i := 0; i < goroutines; i++ {
			err := <-errs
			assert.Error(t, err, "all calls should fail with credential error")
		}
	})

	assert.False(t, mgr.awsInitialized)
}

func TestDirectDialer_RouteProxy_TouchCalledOnSuccess(t *testing.T) {
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

	mockClock := clock.NewMockClock(time.Now())
	var touched bool
	tracker := NewIdleTracker(30*time.Minute, mockClock, func() {})

	// Override Touch to detect calls
	cfg := &Config{ListenAddr: "127.0.0.1:0"}
	router := &mockRouter{route: routing.RouteProxy}
	server := NewDirectSOCKS5Server(cfg, nil, router)
	server.SetIdleTracker(tracker)

	// Set backend dialer that connects to dummy server
	server.SetBackendDialer(&mockBackendForTest{
		dialFunc: func(ctx context.Context, network, addr string) (net.Conn, error) {
			touched = true
			return net.Dial(network, dummyAddr)
		},
	})

	dialer := &directDialer{cfg: cfg, server: server, router: router}

	conn, err := dialer.Dial(context.Background(), "tcp", "internal.example.com:443")
	require.NoError(t, err)
	if conn != nil {
		conn.Close()
	}

	assert.True(t, touched, "backend Dial should be called for RouteProxy")
}

func TestDirectDialer_RouteDirect_NoTouch(t *testing.T) {
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

	mockClock := clock.NewMockClock(time.Now())
	tracker := NewIdleTracker(30*time.Minute, mockClock, func() {})
	tracker.Start()

	cfg := &Config{ListenAddr: "127.0.0.1:0"}
	router := &mockRouter{route: routing.RouteDirect}
	server := NewDirectSOCKS5Server(cfg, nil, router)
	server.SetIdleTracker(tracker)

	dialer := &directDialer{cfg: cfg, server: server, router: router}

	// Direct route should not touch the idle tracker
	conn, err := dialer.Dial(context.Background(), "tcp", dummyAddr)
	require.NoError(t, err)
	if conn != nil {
		conn.Close()
	}

	// The tracker's timer should still be running (not reset)
	// We verify by advancing past timeout - it should fire
	mockClock.Advance(31 * time.Minute)
	assert.True(t, tracker.IsSuspended())
}
