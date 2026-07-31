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
	"github.com/wadahiro/awsocks/internal/routing"
)

func TestSOCKS5Server_StopClosesListenerDirect(t *testing.T) {
	cfg := newTestConfig()
	server := NewSOCKS5Server(cfg, nil, nil)

	startErr := make(chan error, 1)
	go func() {
		startErr <- server.Start()
	}()

	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener)

	addr := listener.Addr().String()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	require.NoError(t, err)
	conn.Close()

	server.Stop()

	select {
	case err := <-startErr:
		assert.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return after Stop()")
	}

	_, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
	assert.Error(t, err)
}

func TestSOCKS5Server_StopBeforeStartDirect(t *testing.T) {
	server := NewSOCKS5Server(newTestConfig(), nil, nil)
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

func TestSOCKS5Server_UsesBackendWhenProvided(t *testing.T) {
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

	dialCalled := false
	var dialedAddr string
	mockBe := &mockBackendForTest{
		dialFunc: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialCalled = true
			dialedAddr = addr
			return net.Dial(network, dummyAddr)
		},
	}

	server := NewSOCKS5Server(newTestConfig(), nil, nil)
	server.SetBackendDialer(mockBe)

	go func() {
		server.Start()
	}()

	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener)

	addr := listener.Addr().String()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	require.NoError(t, err)

	_, err = conn.Write([]byte{0x05, 0x01, 0x00})
	require.NoError(t, err)

	buf := make([]byte, 2)
	_, err = conn.Read(buf)
	require.NoError(t, err)

	request := []byte{0x05, 0x01, 0x00, 0x03, 0x0b}
	request = append(request, []byte("example.com")...)
	request = append(request, 0x00, 0x50)
	_, err = conn.Write(request)
	require.NoError(t, err)

	response := make([]byte, 10)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	conn.Read(response)

	conn.Close()
	server.Stop()

	assert.True(t, dialCalled)
	assert.Equal(t, "example.com:80", dialedAddr)
}

func TestSOCKS5Server_FallsBackToDirectWhenNoBackend(t *testing.T) {
	server := NewSOCKS5Server(newTestConfig(), nil, nil)

	go func() {
		server.Start()
	}()

	time.Sleep(100 * time.Millisecond)

	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener)

	addr := listener.Addr().String()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	require.NoError(t, err)

	_, err = conn.Write([]byte{0x05, 0x01, 0x00})
	require.NoError(t, err)

	buf := make([]byte, 2)
	_, err = conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, byte(0x05), buf[0])
	assert.Equal(t, byte(0x00), buf[1])

	conn.Close()
	server.Stop()
}

// mockLazyInitForDirect implements LazyInitializer
type mockLazyInitForDirect struct {
	initDone    chan struct{}
	initErr     error
	ensureCalls int32
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

func TestDial_RouteProxy_WaitsForInit(t *testing.T) {
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
	server := NewSOCKS5Server(newTestConfig(), router, nil)

	mock := newMockLazyInitForDirect()
	server.SetLazyInitializer(mock)
	server.SetBackendDialer(&mockBackendForTest{
		dialFunc: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, dummyAddr)
		},
	})

	type dialResult struct {
		conn net.Conn
		err  error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := server.dial(context.Background(), "tcp", "internal.example.com:443")
		resultCh <- dialResult{conn: conn, err: err}
	}()

	select {
	case <-resultCh:
		t.Fatal("Dial should not return before init completes")
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
		t.Fatal("Dial did not return after init completed")
	}
}

func TestDial_RouteProxy_PreConnectOverrideSkipsWait(t *testing.T) {
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

	router := &mockRouter{route: routing.RouteProxy, preConnectRoute: routing.RouteDirect}
	server := NewSOCKS5Server(newTestConfig(), router, nil)

	mock := newMockLazyInitForDirect()
	server.SetLazyInitializer(mock)
	// Backend would block forever if used, proving the pre-connect override bypassed it.
	server.SetBackendDialer(&mockBackendForTest{
		dialFunc: func(ctx context.Context, network, addr string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})

	type dialResult struct {
		conn net.Conn
		err  error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := server.dial(context.Background(), "tcp", dummyAddr)
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
		t.Fatal("Dial should return immediately via pre-connect override without waiting for init")
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&mock.ensureCalls))
}

func TestDial_RouteProxy_InitFailure(t *testing.T) {
	router := &mockRouter{route: routing.RouteProxy}
	server := NewSOCKS5Server(newTestConfig(), router, nil)

	mock := newMockLazyInitForDirect()
	server.SetLazyInitializer(mock)

	type dialResult struct {
		conn net.Conn
		err  error
	}
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
		t.Fatal("Dial did not return after init failed")
	}
}

func TestDial_RouteDirect_NoWait(t *testing.T) {
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

	mock := newMockLazyInitForDirect()
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

func TestDial_RouteProxy_ContextCancel(t *testing.T) {
	router := &mockRouter{route: routing.RouteProxy}
	server := NewSOCKS5Server(newTestConfig(), router, nil)

	mock := newMockLazyInitForDirect()
	server.SetLazyInitializer(mock)

	ctx, cancel := context.WithCancel(context.Background())

	type dialResult struct {
		conn net.Conn
		err  error
	}
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
		t.Fatal("Dial did not return after context cancellation")
	}
}

// mockFullBackend implements backend.Backend for testing suspend
type mockFullBackend struct {
	closed   bool
	dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (m *mockFullBackend) Name() string                    { return "mock" }
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

func TestManager_SuspendResumeCycle(t *testing.T) {
	mockClock := clock.NewMockClock(time.Now())

	cfg := &Config{
		ListenAddr:  "127.0.0.1:0",
		IdleTimeout: 30 * time.Minute,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockBe := &mockFullBackend{}

	mgr := &Manager{
		cfg:      cfg,
		initDone: make(chan struct{}),
		clock:    mockClock,
		ctx:      ctx,
		cancel:   cancel,
		backend:  mockBe,
	}

	mgr.awsInitialized = true
	close(mgr.initDone)

	assert.True(t, mgr.IsInitialized())

	mgr.suspend()

	assert.False(t, mgr.IsInitialized())

	select {
	case <-mgr.InitDone():
		t.Fatal("initDone should not be closed after suspend")
	default:
	}

	assert.Nil(t, mgr.initErr)
	assert.True(t, mockBe.closed)
	assert.Nil(t, mgr.backend)
}

func TestManager_EnsureInitialized_ErrorThenRetry_NoPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := &Manager{
		cfg: &Config{
			ListenAddr: "127.0.0.1:0",
		},
		initDone: make(chan struct{}),
		clock:    clock.RealClock{},
		ctx:      ctx,
		cancel:   cancel,
		initializeProxyFn: func(ctx context.Context) error {
			return fmt.Errorf("mock initialization error")
		},
	}

	err1 := mgr.EnsureInitialized(context.Background())
	require.Error(t, err1)

	assert.False(t, mgr.awsInitialized)

	assert.NotPanics(t, func() {
		err2 := mgr.EnsureInitialized(context.Background())
		assert.Error(t, err2)
	})
}

func TestManager_EnsureInitialized_ConcurrentAfterSuspend_NoPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := &Manager{
		cfg: &Config{
			ListenAddr: "127.0.0.1:0",
		},
		initDone: make(chan struct{}),
		clock:    clock.RealClock{},
		ctx:      ctx,
		cancel:   cancel,
		initializeProxyFn: func(ctx context.Context) error {
			return fmt.Errorf("mock initialization error")
		},
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
			assert.Error(t, err)
		}
	})

	assert.False(t, mgr.awsInitialized)
}

func TestDial_RouteProxy_TouchCalledOnSuccess(t *testing.T) {
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

	router := &mockRouter{route: routing.RouteProxy}
	server := NewSOCKS5Server(newTestConfig(), router, nil)
	server.SetIdleTracker(tracker)
	server.SetBackendDialer(&mockBackendForTest{
		dialFunc: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, dummyAddr)
		},
	})

	conn, err := server.dial(context.Background(), "tcp", "internal.example.com:443")
	require.NoError(t, err)
	if conn != nil {
		conn.Close()
	}
}

func TestDial_RouteDirect_NoTouch(t *testing.T) {
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

	router := &mockRouter{route: routing.RouteDirect}
	server := NewSOCKS5Server(newTestConfig(), router, nil)
	server.SetIdleTracker(tracker)

	conn, err := server.dial(context.Background(), "tcp", dummyAddr)
	require.NoError(t, err)
	if conn != nil {
		conn.Close()
	}

	mockClock.Advance(31 * time.Minute)
	assert.True(t, tracker.IsSuspended())
}
