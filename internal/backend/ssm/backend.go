package ssm

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/wadahiro/awsocks/internal/backend/ssm/datachannel"
	ec2pkg "github.com/wadahiro/awsocks/internal/ec2"
	"github.com/wadahiro/awsocks/internal/log"
	"golang.org/x/crypto/ssh"
)

var logger = log.For(log.ComponentSSM)

// State represents the current state of the backend
type State int

const (
	StateIdle State = iota
	StateStartingEC2 // EC2 instance is being started (lazy connection)
	StateConnecting
	StateHandshaking
	StateActive
	StateReconnecting
	StateError
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateStartingEC2:
		return "starting-ec2"
	case StateConnecting:
		return "connecting"
	case StateHandshaking:
		return "handshaking"
	case StateActive:
		return "active"
	case StateReconnecting:
		return "reconnecting"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// LogFunc is a callback function for logging from the backend
type LogFunc func(level, format string, args ...interface{})

// Config holds MuxSSH backend configuration
type Config struct {
	InstanceID  string
	Region      string
	SSHUser     string
	SSHKeyPath  string
	SSHPassword string

	// SSH keepalive interval (0 to disable)
	SSHKeepaliveInterval time.Duration

	// EC2 auto-start settings (for lazy connection)
	AutoStartEC2 bool          // Enable EC2 auto-start on first Dial
	EC2Client    ec2pkg.Client // EC2 client for instance management (optional)

	// Custom DialContext for WebSocket connections (nil = default)
	DialContextFn datachannel.DialContextFunc

	// Log callback (optional, for VM mode to forward logs to host)
	LogFunc LogFunc
}

// Backend implements SSH over SSM DataChannel with smux multiplexing
type Backend struct {
	config    *Config
	ssmClient SSMClient

	// DataChannel components
	dataChannel *datachannel.DataChannel

	// Data bridge (net.Pipe + transfer goroutine lifecycle)
	bridge *dataBridge

	// SSH client (lock-free read via atomic.Pointer)
	sshClient atomic.Pointer[ssh.Client]
	sshConfig *ssh.ClientConfig

	// State management (lock-free read via atomic.Int32)
	state       atomic.Int32   // stores State values
	stateMu     sync.Mutex     // protects stateNotify close-and-recreate only
	stateNotify chan struct{}   // closed on state change to broadcast to all waiters

	// Credentials (lock-free read via atomic.Value)
	credentials atomic.Value // stores aws.Credentials

	// Connection tracking
	openChannels int64

	ctx    context.Context
	cancel context.CancelFunc
}

// New creates a new MuxSSH backend
func New(cfg *Config, ssmClient SSMClient) *Backend {
	b := &Backend{
		config:      cfg,
		ssmClient:   ssmClient,
		stateNotify: make(chan struct{}),
	}
	b.state.Store(int32(StateIdle))
	return b
}

// log helper methods that use callback if available, otherwise use standard logger
func (b *Backend) logInfo(format string, args ...interface{}) {
	if b.config.LogFunc != nil {
		b.config.LogFunc("info", format, args...)
	} else {
		logger.Info(fmt.Sprintf(format, args...))
	}
}

func (b *Backend) logDebug(format string, args ...interface{}) {
	if b.config.LogFunc != nil {
		b.config.LogFunc("debug", format, args...)
	} else {
		logger.Debug(fmt.Sprintf(format, args...))
	}
}

func (b *Backend) logError(format string, args ...interface{}) {
	if b.config.LogFunc != nil {
		b.config.LogFunc("error", format, args...)
	} else {
		logger.Error(fmt.Sprintf(format, args...))
	}
}

func (b *Backend) logWarn(format string, args ...interface{}) {
	if b.config.LogFunc != nil {
		b.config.LogFunc("warn", format, args...)
	} else {
		logger.Warn(fmt.Sprintf(format, args...))
	}
}

// getState returns the current state (lock-free).
func (b *Backend) getState() State {
	return State(b.state.Load())
}

// setState atomically stores the new state and broadcasts to all waiters.
func (b *Backend) setState(newState State) {
	b.state.Store(int32(newState))
	b.stateMu.Lock()
	old := b.stateNotify
	b.stateNotify = make(chan struct{})
	b.stateMu.Unlock()
	close(old)
}

// Name returns the backend name
func (b *Backend) Name() string {
	return "ssm"
}

// Start initializes the backend
func (b *Backend) Start(ctx context.Context) error {
	b.ctx, b.cancel = context.WithCancel(ctx)

	// Parse SSH key if provided
	if b.config.SSHKeyPath != "" {
		sshConfig, err := NewSSHClientConfig(b.config)
		if err != nil {
			return fmt.Errorf("failed to create SSH config: %w", err)
		}
		b.sshConfig = sshConfig
	}

	return nil
}

// Dial establishes a connection to the target address via SSH direct-tcpip channel
func (b *Backend) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	b.logDebug("Dial network=%s address=%s", network, address)

	// Check state and potentially trigger lazy connection (lock-free read)
	state := b.getState()
	switch {
	case state == StateIdle:
		// CAS to atomically transition Idle → Connecting (only one goroutine wins)
		if b.state.CompareAndSwap(int32(StateIdle), int32(StateConnecting)) {
			b.logInfo("Lazy connection: starting connection on first proxy request")
			b.setState(StateConnecting)

			// Start connection (including EC2 auto-start if configured)
			go b.connectWithEC2Start()
		}

		// Wait for active state
		if err := b.waitForActive(ctx); err != nil {
			return nil, fmt.Errorf("lazy connection failed: %w", err)
		}
	case state == StateReconnecting || state == StateConnecting || state == StateStartingEC2 || state == StateHandshaking:
		// Wait for connection to complete
		if err := b.waitForActive(ctx); err != nil {
			return nil, fmt.Errorf("backend not ready: %w", err)
		}
	case state == StateError:
		return nil, fmt.Errorf("backend in error state")
	default:
		// StateActive - proceed
	}

	client := b.sshClient.Load()
	if client == nil {
		return nil, fmt.Errorf("SSH client not connected")
	}

	// Run SSH Dial in a goroutine and respect context cancellation
	type dialResult struct {
		conn net.Conn
		err  error
	}
	resultCh := make(chan dialResult, 1)

	go func() {
		conn, err := client.Dial(network, address)
		resultCh <- dialResult{conn, err}
	}()

	// Use a shorter timeout for SSH dial to avoid long hangs
	dialTimeout := 15 * time.Second
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("SSH dial cancelled: %w", ctx.Err())
	case <-time.After(dialTimeout):
		return nil, fmt.Errorf("SSH dial timeout after %v for %s", dialTimeout, address)
	case result := <-resultCh:
		if result.err != nil {
			b.logDebug("Dial failed address=%s error=%v", address, result.err)
			return nil, fmt.Errorf("SSH dial failed: %w", result.err)
		}
		channels := atomic.AddInt64(&b.openChannels, 1)
		b.logDebug("Dial success address=%s openChannels=%d", address, channels)
		return &trackedConn{Conn: result.conn, backend: b, address: address}, nil
	}
}

// waitForActive waits for the backend to become active
func (b *Backend) waitForActive(ctx context.Context) error {
	return b.waitForActiveWithTimeout(ctx, 6*time.Minute) // Allow time for EC2 start + SSM connection
}

// waitForActiveWithTimeout waits for the backend to become active with a specified timeout.
// Uses broadcast notification via stateNotify channel (no polling needed).
func (b *Backend) waitForActiveWithTimeout(ctx context.Context, d time.Duration) error {
	timeout := time.After(d)
	for {
		state := b.getState()
		if state == StateActive {
			return nil
		}
		if state == StateError {
			return fmt.Errorf("backend entered error state")
		}

		// Get current notification channel
		b.stateMu.Lock()
		waitCh := b.stateNotify
		b.stateMu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for backend to become active")
		case <-waitCh:
			// State changed, re-check in next iteration
		}
	}
}

// SetCredentials stores credentials without triggering a connection.
// Used for lazy connection mode where connection is deferred until first Dial.
func (b *Backend) SetCredentials(creds aws.Credentials) {
	b.credentials.Store(creds)
}

// OnCredentialUpdate handles credential refresh.
func (b *Backend) OnCredentialUpdate(creds aws.Credentials) error {
	b.credentials.Store(creds)

	if b.getState() == StateIdle {
		// CAS to atomically transition Idle → Connecting
		if b.state.CompareAndSwap(int32(StateIdle), int32(StateConnecting)) {
			b.setState(StateConnecting)
			go b.connect()
		}
	}
	// For StateActive: SSH connection doesn't need to reconnect on credential refresh.
	// The SSM session is already established and SSH uses the existing connection.
	// Only reconnect if the connection is actually broken (handled by isConnectionError).
	return nil
}

// connectWithEC2Start handles lazy connection including EC2 auto-start if configured
func (b *Backend) connectWithEC2Start() {
	// EC2 auto-start if configured
	if b.config.AutoStartEC2 && b.config.EC2Client != nil {
		b.setState(StateStartingEC2)

		b.logInfo("Checking EC2 instance state for auto-start... instance=%s", b.config.InstanceID)

		instMgr := ec2pkg.NewInstanceManager(b.config.EC2Client)
		state, err := instMgr.GetInstanceState(b.ctx, b.config.InstanceID)
		if err != nil {
			b.logError("Failed to get instance state: %v", err)
			b.setState(StateError)
			return
		}

		if state == "stopped" {
			b.logInfo("Instance is stopped, starting... instance=%s", b.config.InstanceID)
			if err := instMgr.StartAndWait(b.ctx, b.config.InstanceID, 5*time.Minute); err != nil {
				b.logError("Failed to start instance: %v", err)
				b.setState(StateError)
				return
			}
			b.logInfo("Instance is now running instance=%s", b.config.InstanceID)
		}

		b.setState(StateConnecting)

		// Wait for SSM agent to become online after EC2 start
		if err := b.waitForSSMAgent(); err != nil {
			b.logError("SSM agent not ready: %v", err)
			b.setState(StateError)
			return
		}
	}

	// Now proceed with normal connection
	b.connect()
}

// Default polling interval for waitForSSMAgent
var ssmAgentPollInterval = 5 * time.Second

// SSH handshake parameters (package-level vars for testing)
var (
	sshMaxRetries       = 10
	sshRetryInterval    = 500 * time.Millisecond
	sshHandshakeTimeout = 30 * time.Second
)

// waitForSSMAgent polls DescribeInstanceInformation until PingStatus is "Online"
func (b *Backend) waitForSSMAgent() error {
	return b.waitForSSMAgentWithTimeout(2 * time.Minute)
}

// waitForSSMAgentWithTimeout polls DescribeInstanceInformation with a configurable timeout
func (b *Backend) waitForSSMAgentWithTimeout(timeout time.Duration) error {
	ticker := time.NewTicker(ssmAgentPollInterval)
	defer ticker.Stop()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// Check immediately before first tick
	output, err := b.ssmClient.DescribeInstanceInformation(b.ctx, &DescribeInstanceInformationInput{
		InstanceID: b.config.InstanceID,
	})
	if err == nil && output.PingStatus == "Online" {
		b.logInfo("SSM agent is online instance=%s", b.config.InstanceID)
		return nil
	}

	b.logInfo("Waiting for SSM agent to become online... instance=%s", b.config.InstanceID)

	for {
		select {
		case <-b.ctx.Done():
			return b.ctx.Err()
		case <-timer.C:
			return fmt.Errorf("timeout waiting for SSM agent to become online after %v", timeout)
		case <-ticker.C:
			output, err := b.ssmClient.DescribeInstanceInformation(b.ctx, &DescribeInstanceInformationInput{
				InstanceID: b.config.InstanceID,
			})
			if err != nil {
				b.logWarn("DescribeInstanceInformation failed: %v", err)
				continue
			}
			if output.PingStatus == "Online" {
				b.logInfo("SSM agent is now online instance=%s", b.config.InstanceID)
				return nil
			}
			b.logDebug("SSM agent not ready yet pingStatus=%s instance=%s", output.PingStatus, b.config.InstanceID)
		}
	}
}

// connect establishes the full connection stack
func (b *Backend) connect() {
	startTime := time.Now()
	const maxRetries = 12
	const retryInterval = 10 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		b.logInfo("SSM connection attempt %d/%d instance=%s", attempt, maxRetries, b.config.InstanceID)
		err := b.tryConnect()
		if err == nil {
			b.logInfo("SSM connection established duration=%v instance=%s", time.Since(startTime), b.config.InstanceID)
			return
		}

		b.logInfo("SSM connection attempt %d failed: %v", attempt, err)
		if attempt < maxRetries {
			time.Sleep(retryInterval)
		}
	}

	b.logError("SSM connection failed after %d attempts", maxRetries)
	b.setState(StateError)
}

// tryConnect attempts a single connection
func (b *Backend) tryConnect() error {
	// Create SSM session
	b.logInfo("Starting SSM session...")
	session, err := b.startSSMSession()
	if err != nil {
		return fmt.Errorf("failed to start SSM session: %w", err)
	}
	b.logInfo("SSM session created sessionID=%s", session.SessionID)

	// Update state to handshaking
	b.setState(StateHandshaking)

	// Open DataChannel
	// Capture in local variable to avoid nil pointer dereference if cleanup()
	// runs concurrently (e.g., idle timeout calling Backend.Close()).
	dc := datachannel.NewDataChannel()
	b.dataChannel = dc
	if b.config.DialContextFn != nil {
		dc.SetDialContextFn(b.config.DialContextFn)
	}
	dc.SetClientVersion("1.0.0")

	// Wait for handshake completion
	handshakeDone := make(chan struct{})
	var handshakeOnce sync.Once
	dc.SetOnHandshakeComplete(func() {
		handshakeOnce.Do(func() { close(handshakeDone) })
	})

	// Set disconnect handler before Open() to avoid race window where
	// goroutines start but handler is not yet registered.
	dc.SetOnDisconnect(func() {
		b.handleDisconnect()
	})

	b.logInfo("Opening DataChannel WebSocket... streamURL=%s", session.StreamURL[:min(len(session.StreamURL), 80)])
	if err := dc.Open(b.ctx, session.StreamURL); err != nil {
		return fmt.Errorf("failed to open data channel: %w", err)
	}
	b.logInfo("DataChannel WebSocket connected")

	// Send OpenDataChannel message to authenticate (as JSON, not binary)
	openMsg := map[string]string{
		"MessageSchemaVersion": "1.0",
		"RequestId":            session.SessionID,
		"TokenValue":           session.TokenValue,
	}
	if err := dc.SendJSON(openMsg); err != nil {
		dc.Close()
		return fmt.Errorf("failed to send open message: %w", err)
	}
	b.logInfo("OpenDataChannel message sent, waiting for handshake...")

	// Wait for handshake or timeout
	select {
	case <-handshakeDone:
		// Handshake complete
		b.logInfo("SSM handshake complete")
	case <-time.After(30 * time.Second):
		dc.Close()
		return fmt.Errorf("handshake timeout")
	case <-b.ctx.Done():
		dc.Close()
		return b.ctx.Err()
	}

	// Set up error handler for DataChannel
	dc.SetOnError(func(err error) {
		b.logError("DataChannel error: %v", err)
	})

	// Set up net.Pipe bridge for SSH <-> DataChannel
	bridge, bridgeCtx := newDataBridge(b.ctx)
	b.bridge = bridge

	// Set up DataChannel output callback: DataChannel → dcConn
	// Capture bridge in closure to avoid nil-check races on b.bridge.
	dc.SetOnOutputData(func(data []byte) {
		if _, err := bridge.dcConn.Write(data); err != nil {
			select {
			case <-b.ctx.Done():
			default:
				logger.Debug("DataChannel output write error", "error", err)
			}
		}
	})

	// Start transfer goroutine: dcConn → DataChannel
	bridge.startTransfer(bridgeCtx, dc, func(format string, args ...interface{}) {
		logger.Debug(fmt.Sprintf(format, args...))
	})

	// Establish SSH over the bridge (sshConn side of net.Pipe)
	b.logInfo("Starting SSH handshake...")
	if err := b.connectSSH(dc); err != nil {
		bridge.Close()
		b.bridge = nil
		dc.Close()
		return fmt.Errorf("failed to connect SSH: %w", err)
	}
	b.logInfo("SSH connection established")

	// Wait a bit before accepting connections to let SSM stabilize
	b.logDebug("Connection established, waiting for SSM to stabilize...")
	time.Sleep(500 * time.Millisecond)

	b.setState(StateActive)
	b.logInfo("Backend is now active")

	// Start monitoring for connection loss and auto-reconnect
	go b.monitorConnection()

	return nil
}

// startSSMSession creates a new SSM session
func (b *Backend) startSSMSession() (*SSMSession, error) {
	creds, _ := b.credentials.Load().(aws.Credentials)

	if b.ssmClient == nil {
		return nil, fmt.Errorf("SSM client not configured")
	}

	return StartSSMSession(b.ctx, b.ssmClient, b.config.InstanceID, b.config.Region, creds)
}


// connectSSH establishes SSH over the net.Pipe bridge
// It retries SSH handshake with short intervals since SSM agent may not be ready immediately.
// Each attempt has a timeout to prevent hanging when the WebSocket disconnects
// (net.Pipe does not support SetDeadline, so ssh.ClientConfig.Timeout is ineffective).
func (b *Backend) connectSSH(dc *datachannel.DataChannel) error {
	if b.sshConfig == nil {
		return fmt.Errorf("SSH config not initialized")
	}

	type sshResult struct {
		conn  ssh.Conn
		chans <-chan ssh.NewChannel
		reqs  <-chan *ssh.Request
		err   error
	}

	var lastErr error
	for attempt := 1; attempt <= sshMaxRetries; attempt++ {
		bridge := b.bridge

		// Run SSH handshake in a goroutine with timeout
		resultCh := make(chan sshResult, 1)
		go func() {
			c, ch, r, err := ssh.NewClientConn(bridge.sshConn, "ssm:22", b.sshConfig)
			resultCh <- sshResult{c, ch, r, err}
		}()

		timer := time.NewTimer(sshHandshakeTimeout)
		select {
		case result := <-resultCh:
			timer.Stop()
			if result.err == nil {
				client := ssh.NewClient(result.conn, result.chans, result.reqs)
				b.sshClient.Store(client)
				if attempt > 1 {
					b.logDebug("SSH handshake succeeded on attempt %d", attempt)
				}
				return nil
			}
			lastErr = result.err
		case <-timer.C:
			// Timeout: close bridge to unblock the goroutine
			bridge.Close()
			<-resultCh // wait for goroutine to finish (pipe closed, so it returns quickly)
			lastErr = fmt.Errorf("SSH handshake timeout after %v", sshHandshakeTimeout)
		case <-b.ctx.Done():
			// Context cancelled: close bridge to unblock the goroutine
			bridge.Close()
			<-resultCh
			return b.ctx.Err()
		}

		if attempt < sshMaxRetries {
			// Abort retries if DataChannel is already closed (e.g., WebSocket disconnected
			// during handshake). Without this check, each retry would wait for the full
			// sshHandshakeTimeout since no SSH response can arrive over a dead channel.
			if !dc.IsOpen() {
				return fmt.Errorf("DataChannel closed during SSH handshake")
			}

			b.logDebug("SSH handshake attempt %d failed: %v, retrying...", attempt, lastErr)
			time.Sleep(sshRetryInterval)

			// Recreate the bridge for retry since SSH handshake corrupts the connection.
			// The DataChannel output callback is re-registered with the new bridge's dcConn.
			newBridge, bridgeCtx := newDataBridge(b.ctx)
			b.bridge = newBridge

			dc.SetOnOutputData(func(data []byte) {
				if _, err := newBridge.dcConn.Write(data); err != nil {
					select {
					case <-b.ctx.Done():
					default:
						logger.Debug("DataChannel output write error", "error", err)
					}
				}
			})

			newBridge.startTransfer(bridgeCtx, dc, func(format string, args ...interface{}) {
				logger.Debug(fmt.Sprintf(format, args...))
			})
		}
	}

	return fmt.Errorf("SSH handshake failed after %d attempts: %w", sshMaxRetries, lastErr)
}

// monitorConnection monitors the SSH connection and triggers reconnect on failure.
// When SSHKeepaliveInterval > 0, it sends periodic SSH keepalive requests to prevent
// idle WebSocket disconnections by network infrastructure (proxies/firewalls).
func (b *Backend) monitorConnection() {
	client := b.sshClient.Load()
	if client == nil {
		return
	}

	// Monitor SSH connection close in a separate goroutine
	disconnectCh := make(chan error, 1)
	go func() {
		disconnectCh <- client.Wait()
	}()

	interval := b.config.SSHKeepaliveInterval
	if interval <= 0 {
		// No keepalive: just wait for disconnect
		select {
		case <-b.ctx.Done():
			b.logDebug("SSH connection closed (context cancelled)")
			return
		case err := <-disconnectCh:
			b.logWarn("SSH connection lost, will reconnect... error=%v", err)
			b.triggerReconnect()
			return
		}
	}

	// Keepalive enabled
	b.logInfo("SSH keepalive enabled interval=%v", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	const keepaliveTimeout = 15 * time.Second

	for {
		select {
		case <-b.ctx.Done():
			b.logDebug("SSH connection closed (context cancelled)")
			return
		case err := <-disconnectCh:
			b.logWarn("SSH connection lost, will reconnect... error=%v", err)
			b.triggerReconnect()
			return
		case <-ticker.C:
			// Send keepalive request with timeout
			replyCh := make(chan error, 1)
			go func() {
				_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
				replyCh <- err
			}()

			select {
			case <-b.ctx.Done():
				b.logDebug("SSH connection closed during keepalive (context cancelled)")
				return
			case err := <-replyCh:
				if err != nil {
					b.logWarn("SSH keepalive failed, connection dead error=%v", err)
					b.triggerReconnect()
					return
				}
				b.logDebug("SSH keepalive ok")
			case <-time.After(keepaliveTimeout):
				b.logWarn("SSH keepalive timeout after %v, connection dead", keepaliveTimeout)
				b.triggerReconnect()
				return
			}
		}
	}
}

// handleDisconnect dispatches DataChannel disconnect events based on current state.
// This replaces the pattern of swapping onDisconnect callbacks before/after SSH handshake.
func (b *Backend) handleDisconnect() {
	state := b.getState()
	switch state {
	case StateHandshaking:
		b.logWarn("DataChannel disconnected during SSH handshake")
		if bridge := b.bridge; bridge != nil {
			bridge.Close()
			b.bridge = nil
		}
		if dc := b.dataChannel; dc != nil {
			dc.Close()
		}
	case StateActive:
		b.logWarn("DataChannel disconnected, triggering reconnect...")
		b.triggerReconnect()
	default:
		b.logDebug("DataChannel disconnected in state %s", state)
	}
}

// triggerReconnect initiates a reconnection.
// Uses CAS to ensure only one goroutine transitions to Reconnecting.
func (b *Backend) triggerReconnect() {
	// Only transition if not already reconnecting/connecting
	currentState := b.getState()
	if currentState == StateReconnecting || currentState == StateConnecting {
		b.logDebug("Already reconnecting, skipping state=%s", currentState.String())
		return
	}

	b.setState(StateReconnecting)

	b.logDebug("Cleaning up old connection...")

	// Cleanup old connection
	b.cleanup()

	// Note: Do NOT clear failed hosts cache on reconnect
	// The "No route to host" errors cause SSM to disconnect,
	// so we need to keep blocking those hosts to prevent reconnect loops

	// Reconnect
	b.logInfo("Starting reconnection...")
	b.setState(StateConnecting)

	go b.connect()
}

// cleanup closes all resources.
// Uses local variable capture to avoid TOCTOU race conditions when called
// concurrently from multiple goroutines (e.g., triggerReconnect and Close).
func (b *Backend) cleanup() {
	if client := b.sshClient.Swap(nil); client != nil {
		client.Close()
	}

	if bridge := b.bridge; bridge != nil {
		b.bridge = nil
		bridge.Close()
	}

	if dc := b.dataChannel; dc != nil {
		b.dataChannel = nil
		dc.Close()
	}
}

// Close releases all resources
func (b *Backend) Close() error {
	if b.cancel != nil {
		b.cancel()
	}

	b.cleanup()

	return nil
}

// State returns the current state
func (b *Backend) State() State {
	return b.getState()
}

// UpdateInstanceID updates the instance ID (used for lazy initialization when instance is resolved later)
func (b *Backend) UpdateInstanceID(instanceID string) {
	b.config.InstanceID = instanceID
	b.logDebug("Instance ID updated instance=%s", instanceID)
}

// SetSSHKeyContent sets SSH key from content bytes (for VM mode)
func (b *Backend) SetSSHKeyContent(keyContent []byte, passphrase string) error {
	var signer ssh.Signer
	var err error

	if passphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(keyContent, []byte(passphrase))
	} else {
		signer, err = ssh.ParsePrivateKey(keyContent)
	}
	if err != nil {
		return fmt.Errorf("failed to parse SSH key: %w", err)
	}

	b.sshConfig = &ssh.ClientConfig{
		User:            b.config.SSHUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}
	return nil
}

// trackedConn wraps a net.Conn to track when it's closed
type trackedConn struct {
	net.Conn
	backend   *Backend
	address   string
	closed    bool
	closeOnce sync.Once
}

func (c *trackedConn) Close() error {
	c.closeOnce.Do(func() {
		c.closed = true
		channels := atomic.AddInt64(&c.backend.openChannels, -1)
		logger.Debug("Connection closed", "address", c.address, "openChannels", channels)
	})
	return c.Conn.Close()
}
