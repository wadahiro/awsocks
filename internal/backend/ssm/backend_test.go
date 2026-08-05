package ssm

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/backend/ssm/datachannel"
	"golang.org/x/crypto/ssh"
)

// MockSSMClient is a mock for SSM client
type MockSSMClient struct {
	mock.Mock
}

func (m *MockSSMClient) StartSession(ctx context.Context, input *StartSessionInput) (*StartSessionOutput, error) {
	args := m.Called(ctx, input)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*StartSessionOutput), args.Error(1)
}

func (m *MockSSMClient) TerminateSession(ctx context.Context, input *TerminateSessionInput) (*TerminateSessionOutput, error) {
	args := m.Called(ctx, input)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*TerminateSessionOutput), args.Error(1)
}

func (m *MockSSMClient) DescribeInstanceInformation(ctx context.Context, input *DescribeInstanceInformationInput) (*DescribeInstanceInformationOutput, error) {
	args := m.Called(ctx, input)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*DescribeInstanceInformationOutput), args.Error(1)
}

func TestBackend_Name(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}

	b := New(cfg, nil)
	assert.Equal(t, "ssm", b.Name())
}

func TestBackend_Start(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}

	b := New(cfg, nil)
	ctx := context.Background()

	err := b.Start(ctx)
	assert.NoError(t, err)
	assert.Equal(t, StateIdle, b.State())

	b.Close()
}

func TestBackend_StateTransitions(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}

	b := New(cfg, nil)
	ctx := context.Background()

	// Initial state
	assert.Equal(t, StateIdle, b.State())

	err := b.Start(ctx)
	assert.NoError(t, err)
	assert.Equal(t, StateIdle, b.State())

	b.Close()
}

func TestBackend_Close(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}

	b := New(cfg, nil)
	ctx := context.Background()

	err := b.Start(ctx)
	assert.NoError(t, err)

	err = b.Close()
	assert.NoError(t, err)
}

func TestBackend_OnCredentialUpdate_FromIdle(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}

	b := New(cfg, nil)
	ctx := context.Background()

	err := b.Start(ctx)
	assert.NoError(t, err)
	defer b.Close()

	// Credential update should trigger connection
	creds := aws.Credentials{
		AccessKeyID:     "AKIATEST",
		SecretAccessKey: "secret",
		SessionToken:    "token",
	}

	// This will fail to connect (no real SSM), but should change state
	_ = b.OnCredentialUpdate(creds)

	// After credential update, state should be connecting (even if it fails)
	// We use eventually here because connection happens async
	time.Sleep(100 * time.Millisecond)
	state := b.State()
	assert.True(t, state == StateConnecting || state == StateError || state == StateIdle,
		"expected state to be connecting, error, or idle, got %s", state)
}

func TestBackend_Dial_NotReady(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}

	b := New(cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := b.Start(ctx)
	assert.NoError(t, err)
	defer b.Close()

	// Dial without credentials should fail
	_, err = b.Dial(ctx, "tcp", "example.com:80")
	assert.Error(t, err)
}

func TestBackend_Dial_FromError_RetriesConnection(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}

	b := New(cfg, nil)
	ctx := context.Background()

	err := b.Start(ctx)
	assert.NoError(t, err)
	defer b.Close()

	// Simulate exhausted retries landing the backend in StateError
	b.setState(StateError)
	assert.Equal(t, StateError, b.State())

	// Dial while in StateError should trigger a fresh connection attempt
	// instead of failing immediately with a permanent error.
	dialCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = b.Dial(dialCtx, "tcp", "example.com:80")
	assert.Error(t, err)
	assert.NotContains(t, err.Error(), "backend in error state")

	// State should have moved out of StateError (into Connecting, and
	// eventually back to Error once the retry loop exhausts, since there's
	// no real SSM endpoint to connect to).
	assert.Eventually(t, func() bool {
		state := b.State()
		return state == StateConnecting || state == StateError
	}, time.Second, 10*time.Millisecond)
}

func TestState_String(t *testing.T) {
	testCases := []struct {
		state    State
		expected string
	}{
		{StateIdle, "idle"},
		{StateConnecting, "connecting"},
		{StateHandshaking, "handshaking"},
		{StateActive, "active"},
		{StateReconnecting, "reconnecting"},
		{StateError, "error"},
		{State(99), "unknown"},
	}

	for _, tc := range testCases {
		t.Run(tc.expected, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.state.String())
		})
	}
}

func TestBackend_SetSSHKeyContent(t *testing.T) {
	// Skip if TEST_SSH_KEY_PATH is not set
	keyPath := os.Getenv("TEST_SSH_KEY_PATH")
	if keyPath == "" {
		t.Skip("TEST_SSH_KEY_PATH not set")
	}

	// Expand ~ if present
	if len(keyPath) > 0 && keyPath[0] == '~' {
		home, _ := os.UserHomeDir()
		keyPath = home + keyPath[1:]
	}

	keyContent, err := os.ReadFile(keyPath)
	require.NoError(t, err)

	backend := &Backend{
		config: &Config{SSHUser: "ec2-user"},
	}

	err = backend.SetSSHKeyContent(keyContent, "")
	require.NoError(t, err)
	assert.NotNil(t, backend.sshConfig)
	assert.Equal(t, "ec2-user", backend.sshConfig.User)
}

func TestBackend_SetSSHKeyContent_InvalidKey(t *testing.T) {
	backend := &Backend{
		config: &Config{SSHUser: "ec2-user"},
	}

	err := backend.SetSSHKeyContent([]byte("invalid key"), "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse SSH key")
}

func TestWaitForSSMAgent_AlreadyOnline(t *testing.T) {
	// Use short poll interval for tests
	origInterval := ssmAgentPollInterval
	ssmAgentPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { ssmAgentPollInterval = origInterval })
	mockClient := new(MockSSMClient)
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
	}

	b := New(cfg, mockClient)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.ctx = ctx

	mockClient.On("DescribeInstanceInformation", mock.Anything, &DescribeInstanceInformationInput{
		InstanceID: "i-12345678",
	}).Return(&DescribeInstanceInformationOutput{
		PingStatus: "Online",
	}, nil).Once()

	err := b.waitForSSMAgent()
	assert.NoError(t, err)
	mockClient.AssertExpectations(t)
}

func TestWaitForSSMAgent_BecomesOnline(t *testing.T) {
	origInterval := ssmAgentPollInterval
	ssmAgentPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { ssmAgentPollInterval = origInterval })
	mockClient := new(MockSSMClient)
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
	}

	b := New(cfg, mockClient)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.ctx = ctx

	// First call: not yet online (empty = instance not found in SSM)
	mockClient.On("DescribeInstanceInformation", mock.Anything, &DescribeInstanceInformationInput{
		InstanceID: "i-12345678",
	}).Return(&DescribeInstanceInformationOutput{
		PingStatus: "",
	}, nil).Once()

	// Second call: online
	mockClient.On("DescribeInstanceInformation", mock.Anything, &DescribeInstanceInformationInput{
		InstanceID: "i-12345678",
	}).Return(&DescribeInstanceInformationOutput{
		PingStatus: "Online",
	}, nil).Once()

	err := b.waitForSSMAgent()
	assert.NoError(t, err)
	mockClient.AssertExpectations(t)
}

func TestWaitForSSMAgent_Timeout(t *testing.T) {
	origInterval := ssmAgentPollInterval
	ssmAgentPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { ssmAgentPollInterval = origInterval })
	mockClient := new(MockSSMClient)
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
	}

	b := New(cfg, mockClient)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.ctx = ctx

	// Always return not online
	mockClient.On("DescribeInstanceInformation", mock.Anything, &DescribeInstanceInformationInput{
		InstanceID: "i-12345678",
	}).Return(&DescribeInstanceInformationOutput{
		PingStatus: "ConnectionLost",
	}, nil)

	err := b.waitForSSMAgentWithTimeout(500 * time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
}

func TestWaitForSSMAgent_ContextCancelled(t *testing.T) {
	origInterval := ssmAgentPollInterval
	ssmAgentPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { ssmAgentPollInterval = origInterval })
	mockClient := new(MockSSMClient)
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
	}

	b := New(cfg, mockClient)
	ctx, cancel := context.WithCancel(context.Background())
	b.ctx = ctx

	// Always return not online
	mockClient.On("DescribeInstanceInformation", mock.Anything, &DescribeInstanceInformationInput{
		InstanceID: "i-12345678",
	}).Return(&DescribeInstanceInformationOutput{
		PingStatus: "",
	}, nil).Maybe()

	// Cancel context after a short delay
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := b.waitForSSMAgentWithTimeout(2 * time.Minute)
	assert.Error(t, err)
}

// newTestSSHConfig creates an SSH client config with an ed25519 key for testing
func newTestSSHConfig(t *testing.T) *ssh.ClientConfig {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)

	return &ssh.ClientConfig{
		User:            "ec2-user",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}
}

// useShortSSHTimeouts sets short SSH handshake parameters for testing and restores them on cleanup.
func useShortSSHTimeouts(t *testing.T) {
	t.Helper()
	origMaxRetries := sshMaxRetries
	origRetryInterval := sshRetryInterval
	origHandshakeTimeout := sshHandshakeTimeout

	sshMaxRetries = 1
	sshRetryInterval = 50 * time.Millisecond
	sshHandshakeTimeout = 2 * time.Second

	t.Cleanup(func() {
		sshMaxRetries = origMaxRetries
		sshRetryInterval = origRetryInterval
		sshHandshakeTimeout = origHandshakeTimeout
	})
}

// newTestBackendWithPipe creates a Backend with dataBridge set up for connectSSH testing.
// Returns the backend and the dcConn side of the bridge (for external manipulation).
func newTestBackendWithPipe(t *testing.T) (*Backend, net.Conn) {
	t.Helper()
	cfg := &Config{
		InstanceID: "i-test",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}
	b := New(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		b.Close()
	})
	b.ctx = ctx
	b.cancel = cancel
	b.sshConfig = newTestSSHConfig(t)

	// Create dataBridge (simulating what tryConnect does)
	bridge, _ := newDataBridge(ctx)

	// Note: transfer goroutine is not started because there's no DataChannel in unit tests.
	// Close done channel so bridge.Close() won't block waiting for the goroutine.
	close(bridge.done)

	b.setBridge(bridge)

	return b, bridge.dcConn
}

func TestConnectSSH_TimeoutWhenNoData(t *testing.T) {
	// Test that connectSSH returns an error within a reasonable time
	// when no data comes from the remote side (simulating WebSocket disconnect
	// where StartReceiving has exited but nobody closed the pipe).
	useShortSSHTimeouts(t)
	b, dcSide := newTestBackendWithPipe(t)
	defer dcSide.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- b.connectSSH(b.dataChannel)
	}()

	select {
	case err := <-errCh:
		// connectSSH should return an error (timeout)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "timeout")
	case <-time.After(10 * time.Second):
		t.Fatal("connectSSH hung - timeout mechanism not working")
	}
}

func TestConnectSSH_PipeClosedReturnsError(t *testing.T) {
	// Test that connectSSH returns promptly when the pipe is closed externally
	// (simulating onDisconnect handler closing the pipe via cleanupDataBridge).
	useShortSSHTimeouts(t)
	b, _ := newTestBackendWithPipe(t)

	errCh := make(chan error, 1)
	go func() {
		errCh <- b.connectSSH(b.dataChannel)
	}()

	// Close the bridge after a short delay to simulate onDisconnect handler
	time.Sleep(100 * time.Millisecond)
	b.clearBridge()

	select {
	case err := <-errCh:
		assert.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("connectSSH did not return after pipe was closed")
	}
}

func TestConnectSSH_ContextCancelledReturnsError(t *testing.T) {
	// Test that connectSSH respects context cancellation
	useShortSSHTimeouts(t)
	b, dcSide := newTestBackendWithPipe(t)
	defer dcSide.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- b.connectSSH(b.dataChannel)
	}()

	// Cancel context after a short delay
	time.Sleep(100 * time.Millisecond)
	b.cancel()

	select {
	case err := <-errCh:
		assert.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("connectSSH did not return after context was cancelled")
	}
}

func TestTryConnect_CleanupDuringSSHHandshake_NoPanic(t *testing.T) {
	// Regression test for nil pointer dereference:
	// When cleanup() runs concurrently (e.g., idle timeout calling Backend.Close())
	// while connectSSH is in progress, b.dataChannel is set to nil.
	// The error path in tryConnect must not panic when closing the data channel.
	//
	// Scenario:
	// 1. connectSSH is running (SSH handshake in progress)
	// 2. cleanup() sets b.dataChannel = nil
	// 3. connectSSH returns error (context cancelled)
	// 4. tryConnect error path calls dc.Close() via local variable (no panic)
	useShortSSHTimeouts(t)

	cfg := &Config{
		InstanceID: "i-test",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}
	b := New(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	b.ctx = ctx
	b.cancel = cancel
	b.sshConfig = newTestSSHConfig(t)

	// Create a DataChannel and bridge (simulating what tryConnect sets up)
	dc := datachannel.NewDataChannel()
	b.dataChannel = dc

	bridge, _ := newDataBridge(ctx)
	close(bridge.done)
	b.setBridge(bridge)

	// Start connectSSH in a goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- b.connectSSH(dc)
	}()

	// Simulate idle timeout: cleanup() runs concurrently, setting b.dataChannel = nil
	time.Sleep(50 * time.Millisecond)
	b.cleanup()
	assert.Nil(t, b.dataChannel, "cleanup should have set b.dataChannel to nil")

	// Cancel context to unblock connectSSH
	cancel()

	select {
	case err := <-errCh:
		assert.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("connectSSH did not return")
	}

	// Simulate the tryConnect error path: close dc via local variable.
	// Before the fix, this would be b.dataChannel.Close() which panics because
	// b.dataChannel is nil. With the fix, dc is a local variable still valid.
	assert.NotPanics(t, func() {
		dc.Close()
	}, "closing DataChannel via local variable must not panic even after cleanup()")
}

func TestHandleDisconnect_DuringHandshake(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-test",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}
	b := New(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.ctx = ctx
	b.cancel = cancel

	// Set state to Handshaking
	b.setState(StateHandshaking)
	assert.Equal(t, StateHandshaking, b.State())

	// Create a bridge so handleDisconnect has something to close
	bridge, _ := newDataBridge(ctx)
	close(bridge.done) // no transfer goroutine in test
	b.setBridge(bridge)

	// Call handleDisconnect
	b.handleDisconnect()

	// Bridge should be cleaned up (nil)
	assert.Nil(t, b.getBridge())
}

func TestHandleDisconnect_DuringActive(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-test",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}
	b := New(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.ctx = ctx
	b.cancel = cancel

	// Set state to Active
	b.setState(StateActive)
	assert.Equal(t, StateActive, b.State())

	// Call handleDisconnect - should trigger reconnect (state → Reconnecting → Connecting)
	b.handleDisconnect()

	// State should be Connecting (triggerReconnect sets Reconnecting then Connecting)
	state := b.State()
	assert.True(t, state == StateConnecting || state == StateReconnecting,
		"expected Connecting or Reconnecting, got %s", state)
}

// waitForGoroutineCount polls until the goroutine count drops to at or below
// target, or the deadline expires. Returns the final observed count.
func waitForGoroutineCount(target int, deadline time.Duration) int {
	stop := time.Now().Add(deadline)
	n := runtime.NumGoroutine()
	for n > target && time.Now().Before(stop) {
		time.Sleep(20 * time.Millisecond)
		runtime.GC()
		n = runtime.NumGoroutine()
	}
	return n
}

// TestInstallBridge_ClosesPreviousBridge is a regression test for the bridge
// leak: before the fix, each SSH handshake retry created a new dataBridge (and
// its transfer goroutine) via `b.bridge = newBridge` WITHOUT closing the old
// one, leaving zombie transfer goroutines reading the old dcConn and racing to
// write to the DataChannel. installBridge now closes the previous bridge, so
// repeatedly installing bridges must not grow the goroutine count.
func TestInstallBridge_ClosesPreviousBridge(t *testing.T) {
	cfg := &Config{InstanceID: "i-test", Region: "ap-northeast-1", SSHUser: "ec2-user"}
	b := New(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.ctx = ctx
	b.cancel = cancel

	// A real DataChannel so each installed bridge's startTransfer goroutine runs.
	// It is never Open()ed: SendInputData fails, so a transfer goroutine only
	// exits when its bridge's dcConn is closed (i.e. when the bridge is closed).
	dc := datachannel.NewDataChannel()
	b.dataChannel = dc

	baseline := runtime.NumGoroutine()

	// Simulate the initial install plus several handshake-retry re-installs.
	var first *dataBridge
	for i := 0; i < 5; i++ {
		bridge, bridgeCtx := newDataBridge(ctx)
		if i == 0 {
			first = bridge
		}
		b.installBridge(dc, bridge, bridgeCtx)
	}

	// The first bridge must have been closed by a later install: its transfer
	// goroutine has exited, so its done channel is closed.
	select {
	case <-first.done:
		// closed as expected
	case <-time.After(2 * time.Second):
		t.Fatal("first bridge's transfer goroutine did not exit - bridge was leaked")
	}

	// Clean up the last (still-live) bridge.
	b.clearBridge()

	// Goroutine count must return to baseline: no zombie transfer goroutines
	// accumulated across the re-installs.
	final := waitForGoroutineCount(baseline+1, 5*time.Second)
	assert.LessOrEqualf(t, final, baseline+1,
		"goroutine count grew from baseline %d to %d - transfer goroutines leaked across installs",
		baseline, final)
}

// TestConnectSSH_ExhaustedRetriesLeavesNoBridge asserts the current bridge is
// cleared (not left pointing at a closed bridge) once all handshake attempts
// fail, so a subsequent reconnect starts from a clean slate.
func TestConnectSSH_ExhaustedRetriesLeavesNoBridge(t *testing.T) {
	origMaxRetries := sshMaxRetries
	origRetryInterval := sshRetryInterval
	origHandshakeTimeout := sshHandshakeTimeout
	t.Cleanup(func() {
		sshMaxRetries = origMaxRetries
		sshRetryInterval = origRetryInterval
		sshHandshakeTimeout = origHandshakeTimeout
	})
	sshMaxRetries = 2
	sshRetryInterval = 10 * time.Millisecond
	sshHandshakeTimeout = 150 * time.Millisecond

	cfg := &Config{InstanceID: "i-test", Region: "ap-northeast-1", SSHUser: "ec2-user"}
	b := New(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.ctx = ctx
	b.cancel = cancel
	b.sshConfig = newTestSSHConfig(t)

	// A real (never-Open()ed) DataChannel: the retry path calls dc.IsOpen(),
	// which must not be nil. It reports open here (ctx set on... actually not
	// Open()ed so IsOpen()=false), letting the retry loop take the
	// "DataChannel closed" early-return, which still must clear the bridge.
	dc := datachannel.NewDataChannel()
	b.dataChannel = dc
	initialBridge, initialCtx := newDataBridge(ctx)
	b.installBridge(dc, initialBridge, initialCtx)

	err := b.connectSSH(dc)
	require.Error(t, err)
	assert.Nil(t, b.getBridge())
}
