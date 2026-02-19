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
	"github.com/wadahiro/awsocks/internal/protocol"
	"github.com/wadahiro/awsocks/internal/routing"
)

func TestSOCKS5Server_StopClosesListener(t *testing.T) {
	// Create a mock agent connection (we won't actually use it)
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	server := NewSOCKS5Server("127.0.0.1:0", agentClient, nil)

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

func TestSOCKS5Server_StopBeforeStart(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	server := NewSOCKS5Server("127.0.0.1:0", agentClient, nil)

	// Stop before Start should not panic
	assert.NotPanics(t, func() {
		server.Stop()
	})
}

// mockLazyInit implements LazyInitializer for testing
type mockLazyInit struct {
	initDone    chan struct{}
	initErr     error
	ensureCalls int32 // atomic for thread safety
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

// completeInit simulates successful initialization completion
func (m *mockLazyInit) completeInit() {
	close(m.initDone)
}

// failInit simulates initialization failure
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

func TestSOCKS5Server_dialViaAgent_RouteProxy_WaitsForInit(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	router := &mockRouter{route: routing.RouteProxy}
	server := NewSOCKS5Server("127.0.0.1:0", agentClient, router)

	mock := newMockLazyInit()
	server.SetVMManager(mock)

	// Start mock agent and readFromAgent goroutine (needed for protocol message handling)
	go mockAgent(t, agentServer)
	go server.readFromAgent()

	// dialViaAgent should block until init completes
	resultCh := make(chan connResult, 1)
	go func() {
		conn, err := server.dialViaAgent(context.Background(), "tcp", "internal.example.com:443")
		resultCh <- connResult{conn: conn, err: err}
	}()

	// Verify it's still waiting
	select {
	case <-resultCh:
		t.Fatal("dialViaAgent should not return before init completes")
	case <-time.After(100 * time.Millisecond):
		// Expected: still waiting
	}

	// Complete initialization
	mock.completeInit()

	// Now it should complete successfully
	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		assert.NotNil(t, result.conn)
		if result.conn != nil {
			result.conn.Close()
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dialViaAgent did not return after init completed")
	}
}

func TestSOCKS5Server_dialViaAgent_RouteProxy_InitFailure(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	router := &mockRouter{route: routing.RouteProxy}
	server := NewSOCKS5Server("127.0.0.1:0", agentClient, router)

	mock := newMockLazyInit()
	server.SetVMManager(mock)

	resultCh := make(chan connResult, 1)
	go func() {
		conn, err := server.dialViaAgent(context.Background(), "tcp", "internal.example.com:443")
		resultCh <- connResult{conn: conn, err: err}
	}()

	// Wait for dialViaAgent goroutine to reach the select{} wait
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
		t.Fatal("dialViaAgent did not return after init failed")
	}
}

func TestSOCKS5Server_dialViaAgent_RouteVMDirect_NoInitTrigger(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	router := &mockRouter{route: routing.RouteVMDirect}
	server := NewSOCKS5Server("127.0.0.1:0", agentClient, router)

	mock := newMockLazyInit()
	server.SetVMManager(mock)

	// Start mock agent and readFromAgent goroutine
	go mockAgent(t, agentServer)
	go server.readFromAgent()

	// RouteVMDirect should return immediately without triggering initialization
	resultCh := make(chan connResult, 1)
	go func() {
		conn, err := server.dialViaAgent(context.Background(), "tcp", "example.com:443")
		resultCh <- connResult{conn: conn, err: err}
	}()

	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		assert.NotNil(t, result.conn)
		if result.conn != nil {
			result.conn.Close()
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dialViaAgent should return immediately for RouteVMDirect")
	}

	// EnsureInitialized should NOT have been called
	time.Sleep(50 * time.Millisecond) // wait for any async goroutine to execute
	assert.Equal(t, int32(0), atomic.LoadInt32(&mock.ensureCalls), "EnsureInitialized should not be called for RouteVMDirect")
}

func TestSOCKS5Server_dialViaAgent_RouteProxy_ContextCancel(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	router := &mockRouter{route: routing.RouteProxy}
	server := NewSOCKS5Server("127.0.0.1:0", agentClient, router)

	mock := newMockLazyInit()
	server.SetVMManager(mock)

	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan connResult, 1)
	go func() {
		conn, err := server.dialViaAgent(ctx, "tcp", "internal.example.com:443")
		resultCh <- connResult{conn: conn, err: err}
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
		t.Fatal("dialViaAgent did not return after context cancellation")
	}
}

func TestSOCKS5Server_dialViaAgent_RouteProxy_TouchCalledOnSuccess(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	mockClock := clock.NewMockClock(time.Now())
	tracker := NewIdleTracker(30*time.Minute, mockClock, func() {})
	tracker.Start()

	router := &mockRouter{route: routing.RouteProxy}
	server := NewSOCKS5Server("127.0.0.1:0", agentClient, router)
	server.SetIdleTracker(tracker)

	// Start mock agent and readFromAgent
	go mockAgent(t, agentServer)
	go server.readFromAgent()

	// Init is already complete (no lazy initializer set)
	conn, err := server.dialViaAgent(context.Background(), "tcp", "internal.example.com:443")
	require.NoError(t, err)
	assert.NotNil(t, conn)
	if conn != nil {
		conn.Close()
	}

	// After a successful RouteProxy dial, the timer should have been reset via Touch()
	// Advance 29 minutes - should not fire yet (Touch restarted timer)
	mockClock.Advance(29 * time.Minute)
	assert.False(t, tracker.IsSuspended())

	// Advance past timeout since last Touch
	mockClock.Advance(2 * time.Minute)
	assert.True(t, tracker.IsSuspended())
}

func TestSOCKS5Server_dialViaAgent_RouteVMDirect_NoTouch(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	mockClock := clock.NewMockClock(time.Now())
	tracker := NewIdleTracker(30*time.Minute, mockClock, func() {})
	tracker.Start()

	router := &mockRouter{route: routing.RouteVMDirect}
	server := NewSOCKS5Server("127.0.0.1:0", agentClient, router)
	server.SetIdleTracker(tracker)

	go mockAgent(t, agentServer)
	go server.readFromAgent()

	conn, err := server.dialViaAgent(context.Background(), "tcp", "example.com:443")
	require.NoError(t, err)
	if conn != nil {
		conn.Close()
	}

	// RouteVMDirect should NOT touch the tracker
	// Timer started at 0, advancing 31 minutes should fire
	mockClock.Advance(31 * time.Minute)
	assert.True(t, tracker.IsSuspended())
}
