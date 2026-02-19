package proxy

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/clock"
	"github.com/wadahiro/awsocks/internal/mux"
	"github.com/wadahiro/awsocks/internal/protocol"
	"github.com/wadahiro/awsocks/internal/routing"
)

func newTestConfig() *Config {
	return &Config{
		ListenAddr: "127.0.0.1:0",
	}
}

// dialResult holds the result of a dial attempt for testing
type dialResult struct {
	conn net.Conn
	err  error
}

func TestSOCKS5Server_StopClosesListener(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	agentMux := mux.NewAgentMux(agentClient)
	defer agentMux.Close()

	server := NewSOCKS5Server(newTestConfig(), nil, agentMux)

	startErr := make(chan error, 1)
	go func() {
		startErr <- server.Start()
	}()

	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener, "listener should be set after Start()")

	addr := listener.Addr().String()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	require.NoError(t, err, "should be able to connect to server")
	conn.Close()

	server.Stop()

	select {
	case err := <-startErr:
		assert.Error(t, err, "Start() should return error when listener is closed")
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return after Stop() was called")
	}

	_, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
	assert.Error(t, err, "should not be able to connect after Stop()")
}

func TestSOCKS5Server_StopBeforeStart(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	agentMux := mux.NewAgentMux(agentClient)
	defer agentMux.Close()

	server := NewSOCKS5Server(newTestConfig(), nil, agentMux)

	assert.NotPanics(t, func() {
		server.Stop()
	})
}

// mockLazyInit implements LazyInitializer for testing
type mockLazyInit struct {
	initDone    chan struct{}
	initErr     error
	ensureCalls int32
}

func newMockLazyInit() *mockLazyInit {
	return &mockLazyInit{
		initDone: make(chan struct{}),
	}
}

func (m *mockLazyInit) EnsureInitialized(ctx context.Context) error {
	atomic.AddInt32(&m.ensureCalls, 1)
	return m.initErr
}

func (m *mockLazyInit) InitDone() <-chan struct{} {
	return m.initDone
}

func (m *mockLazyInit) InitError() error {
	return m.initErr
}

func (m *mockLazyInit) completeInit() {
	close(m.initDone)
}

func (m *mockLazyInit) failInit(err error) {
	m.initErr = err
	close(m.initDone)
}

// mockAgent reads protocol messages from conn and responds with ConnectAck
func mockAgent(t *testing.T, conn net.Conn) {
	t.Helper()
	for {
		msg, err := protocol.ReadMessage(conn)
		if err != nil {
			return
		}
		switch msg.Type {
		case protocol.MsgConnect, protocol.MsgConnectDirect:
			ack := &protocol.Message{
				Type:   protocol.MsgConnectAck,
				ConnID: msg.ConnID,
			}
			if err := protocol.WriteMessage(conn, ack); err != nil {
				return
			}
		case protocol.MsgClose:
			// ignore
		}
	}
}

// mockRouter implements routing.Router for testing
type mockRouter struct {
	route         routing.Route
	fallbackRoute routing.Route
}

func (r *mockRouter) Route(host string) routing.Route {
	return r.route
}

func (r *mockRouter) FallbackRoute(current routing.Route) routing.Route {
	return r.fallbackRoute
}

func TestSOCKS5Server_dial_RouteProxy_WaitsForInit(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	agentMux := mux.NewAgentMux(agentClient)
	defer agentMux.Close()

	router := &mockRouter{route: routing.RouteProxy}
	server := NewSOCKS5Server(newTestConfig(), router, agentMux)

	mock := newMockLazyInit()
	server.SetLazyInitializer(mock)

	go mockAgent(t, agentServer)

	// Set backend dialer for after init completes
	dummyListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer dummyListener.Close()
	go func() {
		for {
			conn, err := dummyListener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	dummyAddr := dummyListener.Addr().String()
	server.SetBackendDialer(&mockBackendForTest{
		dialFunc: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, dummyAddr)
		},
	})

	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := server.dial(context.Background(), "tcp", "internal.example.com:443")
		resultCh <- dialResult{conn: conn, err: err}
	}()

	// Verify it's still waiting
	select {
	case <-resultCh:
		t.Fatal("dial should not return before init completes")
	case <-time.After(100 * time.Millisecond):
	}

	mock.completeInit()

	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		assert.NotNil(t, result.conn)
		if result.conn != nil {
			result.conn.Close()
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dial did not return after init completed")
	}
}

func TestSOCKS5Server_dial_RouteProxy_InitFailure(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	agentMux := mux.NewAgentMux(agentClient)
	defer agentMux.Close()

	router := &mockRouter{route: routing.RouteProxy}
	server := NewSOCKS5Server(newTestConfig(), router, agentMux)

	mock := newMockLazyInit()
	server.SetLazyInitializer(mock)

	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := server.dial(context.Background(), "tcp", "internal.example.com:443")
		resultCh <- dialResult{conn: conn, err: err}
	}()

	time.Sleep(100 * time.Millisecond)
	mock.failInit(fmt.Errorf("AWS credentials expired"))

	select {
	case result := <-resultCh:
		assert.Nil(t, result.conn)
		require.Error(t, result.err)
		assert.Contains(t, result.err.Error(), "initialization failed")
		assert.Contains(t, result.err.Error(), "AWS credentials expired")
	case <-time.After(2 * time.Second):
		t.Fatal("dial did not return after init failed")
	}
}

func TestSOCKS5Server_dial_RouteVMDirect_NoInitTrigger(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	agentMux := mux.NewAgentMux(agentClient)
	defer agentMux.Close()

	router := &mockRouter{route: routing.RouteVMDirect}
	server := NewSOCKS5Server(newTestConfig(), router, agentMux)

	mock := newMockLazyInit()
	server.SetLazyInitializer(mock)

	go mockAgent(t, agentServer)

	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := server.dial(context.Background(), "tcp", "example.com:443")
		resultCh <- dialResult{conn: conn, err: err}
	}()

	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		assert.NotNil(t, result.conn)
		if result.conn != nil {
			result.conn.Close()
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dial should return immediately for RouteVMDirect")
	}

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(0), atomic.LoadInt32(&mock.ensureCalls), "EnsureInitialized should not be called for RouteVMDirect")
}

func TestSOCKS5Server_dial_RouteProxy_ContextCancel(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	agentMux := mux.NewAgentMux(agentClient)
	defer agentMux.Close()

	router := &mockRouter{route: routing.RouteProxy}
	server := NewSOCKS5Server(newTestConfig(), router, agentMux)

	mock := newMockLazyInit()
	server.SetLazyInitializer(mock)

	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := server.dial(ctx, "tcp", "internal.example.com:443")
		resultCh <- dialResult{conn: conn, err: err}
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case result := <-resultCh:
		assert.Nil(t, result.conn)
		require.Error(t, result.err)
		assert.ErrorIs(t, result.err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("dial did not return after context cancellation")
	}
}

func TestSOCKS5Server_dial_RouteProxy_TouchCalledOnSuccess(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	agentMux := mux.NewAgentMux(agentClient)
	defer agentMux.Close()

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

	router := &mockRouter{route: routing.RouteProxy}
	server := NewSOCKS5Server(newTestConfig(), router, agentMux)
	server.SetIdleTracker(tracker)
	server.SetBackendDialer(&mockBackendForTest{
		dialFunc: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, dummyAddr)
		},
	})

	conn, err := server.dial(context.Background(), "tcp", "internal.example.com:443")
	require.NoError(t, err)
	assert.NotNil(t, conn)
	if conn != nil {
		conn.Close()
	}

	mockClock.Advance(29 * time.Minute)
	assert.False(t, tracker.IsSuspended())

	mockClock.Advance(2 * time.Minute)
	assert.True(t, tracker.IsSuspended())
}

func TestSOCKS5Server_dial_RouteVMDirect_NoTouch(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	agentMux := mux.NewAgentMux(agentClient)
	defer agentMux.Close()

	mockClock := clock.NewMockClock(time.Now())
	tracker := NewIdleTracker(30*time.Minute, mockClock, func() {})
	tracker.Start()

	router := &mockRouter{route: routing.RouteVMDirect}
	server := NewSOCKS5Server(newTestConfig(), router, agentMux)
	server.SetIdleTracker(tracker)

	go mockAgent(t, agentServer)

	conn, err := server.dial(context.Background(), "tcp", "example.com:443")
	require.NoError(t, err)
	if conn != nil {
		conn.Close()
	}

	mockClock.Advance(31 * time.Minute)
	assert.True(t, tracker.IsSuspended())
}

func TestSOCKS5Server_StopWithoutAgent(t *testing.T) {
	// Test that SOCKS5Server works without agentMux (direct-only mode)
	server := NewSOCKS5Server(newTestConfig(), nil, nil)

	assert.NotPanics(t, func() {
		server.Stop()
	})
}

func TestSOCKS5Server_dial_RouteDirect_NoWait(t *testing.T) {
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

	router := &mockRouter{route: routing.RouteDirect}
	server := NewSOCKS5Server(newTestConfig(), router, nil)

	mock := newMockLazyInit()
	server.SetLazyInitializer(mock)

	conn, err := server.dial(context.Background(), "tcp", dummyAddr)
	require.NoError(t, err)
	assert.NotNil(t, conn)
	if conn != nil {
		conn.Close()
	}

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(0), atomic.LoadInt32(&mock.ensureCalls))
}
