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

	// EC2 auto-start settings (for lazy connection)
	AutoStartEC2 bool          // Enable EC2 auto-start on first Dial
	EC2Client    ec2pkg.Client // EC2 client for instance management (optional)

	// Log callback (optional, for VM mode to forward logs to host)
	LogFunc LogFunc
}

// Backend implements SSH over SSM DataChannel with smux multiplexing
type Backend struct {
	config    *Config
	ssmClient SSMClient

	// DataChannel components
	dataChannel *datachannel.DataChannel

	// net.Pipe for data bridge (SSH <-> DataChannel)
	sshConn net.Conn // SSH Client側
	dcConn  net.Conn // DataChannel側

	// SSH client
	sshClient *ssh.Client
	sshConfig *ssh.ClientConfig
	sshMu     sync.RWMutex

	// State management
	state       State
	stateMu     sync.RWMutex
	stateChange chan struct{}

	// Credentials
	credentials aws.Credentials
	credsMu     sync.RWMutex

	// Connection tracking
	openChannels int64

	ctx    context.Context
	cancel context.CancelFunc
}

// New creates a new MuxSSH backend
func New(cfg *Config, ssmClient SSMClient) *Backend {
	return &Backend{
		config:      cfg,
		ssmClient:   ssmClient,
		state:       StateIdle,
		stateChange: make(chan struct{}, 1),
	}
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

	// Check state and potentially trigger lazy connection
	b.stateMu.Lock()
	state := b.state
	if state == StateIdle {
		// Lazy connection: trigger connection on first Dial
		b.logInfo("Lazy connection: starting connection on first proxy request")
		b.state = StateConnecting
		b.stateMu.Unlock()
		b.notifyStateChange()

		// Start connection (including EC2 auto-start if configured)
		go b.connectWithEC2Start()

		// Wait for active state
		if err := b.waitForActive(ctx); err != nil {
			return nil, fmt.Errorf("lazy connection failed: %w", err)
		}
	} else if state == StateReconnecting || state == StateConnecting || state == StateStartingEC2 {
		b.stateMu.Unlock()
		// Wait for connection to complete
		if err := b.waitForActive(ctx); err != nil {
			return nil, fmt.Errorf("backend not ready: %w", err)
		}
	} else if state == StateHandshaking {
		b.stateMu.Unlock()
		if err := b.waitForActive(ctx); err != nil {
			return nil, fmt.Errorf("backend not ready: %w", err)
		}
	} else if state == StateError {
		b.stateMu.Unlock()
		return nil, fmt.Errorf("backend in error state")
	} else {
		// StateActive
		b.stateMu.Unlock()
	}

	b.sshMu.RLock()
	client := b.sshClient
	b.sshMu.RUnlock()

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

// waitForActiveWithTimeout waits for the backend to become active with a specified timeout
func (b *Backend) waitForActiveWithTimeout(ctx context.Context, d time.Duration) error {
	timeout := time.After(d)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for backend to become active")
		case <-b.stateChange:
			b.stateMu.RLock()
			state := b.state
			b.stateMu.RUnlock()

			if state == StateActive {
				return nil
			}
			if state == StateError {
				return fmt.Errorf("backend entered error state")
			}
		case <-time.After(100 * time.Millisecond):
			b.stateMu.RLock()
			state := b.state
			b.stateMu.RUnlock()

			if state == StateActive {
				return nil
			}
			if state == StateError {
				return fmt.Errorf("backend entered error state")
			}
		}
	}
}

// SetCredentials stores credentials without triggering a connection
// Used for lazy connection mode where connection is deferred until first Dial
func (b *Backend) SetCredentials(creds aws.Credentials) {
	b.credsMu.Lock()
	b.credentials = creds
	b.credsMu.Unlock()
}

// OnCredentialUpdate handles credential refresh
func (b *Backend) OnCredentialUpdate(creds aws.Credentials) error {
	b.credsMu.Lock()
	b.credentials = creds
	b.credsMu.Unlock()

	b.stateMu.Lock()
	currentState := b.state
	if currentState == StateIdle {
		// Initial connection - start connecting
		b.state = StateConnecting
		b.stateMu.Unlock()
		b.notifyStateChange()
		go b.connect()
		return nil
	}
	// For StateActive: SSH connection doesn't need to reconnect on credential refresh
	// The SSM session is already established and SSH uses the existing connection
	// Only reconnect if the connection is actually broken (handled by isConnectionError)
	b.stateMu.Unlock()
	return nil
}

// connectWithEC2Start handles lazy connection including EC2 auto-start if configured
func (b *Backend) connectWithEC2Start() {
	// EC2 auto-start if configured
	if b.config.AutoStartEC2 && b.config.EC2Client != nil {
		b.stateMu.Lock()
		b.state = StateStartingEC2
		b.stateMu.Unlock()
		b.notifyStateChange()

		b.logInfo("Checking EC2 instance state for auto-start... instance=%s", b.config.InstanceID)

		instMgr := ec2pkg.NewInstanceManager(b.config.EC2Client)
		state, err := instMgr.GetInstanceState(b.ctx, b.config.InstanceID)
		if err != nil {
			b.logError("Failed to get instance state: %v", err)
			b.setErrorState()
			return
		}

		if state == "stopped" {
			b.logInfo("Instance is stopped, starting... instance=%s", b.config.InstanceID)
			if err := instMgr.StartAndWait(b.ctx, b.config.InstanceID, 5*time.Minute); err != nil {
				b.logError("Failed to start instance: %v", err)
				b.setErrorState()
				return
			}
			b.logInfo("Instance is now running instance=%s", b.config.InstanceID)
		}

		b.stateMu.Lock()
		b.state = StateConnecting
		b.stateMu.Unlock()
		b.notifyStateChange()
	}

	// Now proceed with normal connection
	b.connect()
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

		b.logWarn("SSM connection attempt %d failed: %v", attempt, err)
		if attempt < maxRetries {
			time.Sleep(retryInterval)
		}
	}

	b.logError("SSM connection failed after %d attempts", maxRetries)
	b.setErrorState()
}

// tryConnect attempts a single connection
func (b *Backend) tryConnect() error {
	// Create SSM session
	session, err := b.startSSMSession()
	if err != nil {
		return fmt.Errorf("failed to start SSM session: %w", err)
	}

	// Update state to handshaking
	b.stateMu.Lock()
	b.state = StateHandshaking
	b.stateMu.Unlock()
	b.notifyStateChange()

	// Open DataChannel
	b.dataChannel = datachannel.NewDataChannel()
	b.dataChannel.SetClientVersion("1.0.0")

	// Wait for handshake completion
	handshakeDone := make(chan struct{})
	b.dataChannel.SetOnHandshakeComplete(func() {
		close(handshakeDone)
	})

	if err := b.dataChannel.Open(b.ctx, session.StreamURL); err != nil {
		return fmt.Errorf("failed to open data channel: %w", err)
	}

	// Send OpenDataChannel message to authenticate (as JSON, not binary)
	openMsg := map[string]string{
		"MessageSchemaVersion": "1.0",
		"RequestId":            session.SessionID,
		"TokenValue":           session.TokenValue,
	}
	if err := b.dataChannel.SendJSON(openMsg); err != nil {
		b.dataChannel.Close()
		return fmt.Errorf("failed to send open message: %w", err)
	}

	// Wait for handshake or timeout
	select {
	case <-handshakeDone:
		// Handshake complete
		b.logDebug("SSM handshake complete")
	case <-time.After(30 * time.Second):
		b.dataChannel.Close()
		return fmt.Errorf("handshake timeout")
	case <-b.ctx.Done():
		b.dataChannel.Close()
		return b.ctx.Err()
	}

	// Set up error handler for DataChannel
	b.dataChannel.SetOnError(func(err error) {
		b.logError("DataChannel error: %v", err)
	})

	// NOTE: Disconnect handler is set AFTER SSH handshake succeeds
	// to avoid false reconnects during SSH retry

	// Set up net.Pipe bridge for SSH <-> DataChannel (with output callback)
	if err := b.setupDataBridge(true); err != nil {
		b.dataChannel.Close()
		return fmt.Errorf("failed to setup data bridge: %w", err)
	}

	// Establish SSH over the bridge (sshConn side of net.Pipe)
	if err := b.connectSSH(); err != nil {
		b.cleanupDataBridge()
		b.dataChannel.Close()
		return fmt.Errorf("failed to connect SSH: %w", err)
	}

	// Now that SSH is connected, set up disconnect handler for auto-reconnect
	b.dataChannel.SetOnDisconnect(func() {
		b.logWarn("DataChannel disconnected, triggering reconnect...")
		b.triggerReconnect()
	})

	// Wait a bit before accepting connections to let SSM stabilize
	b.logDebug("Connection established, waiting for SSM to stabilize...")
	time.Sleep(500 * time.Millisecond)

	b.stateMu.Lock()
	b.state = StateActive
	b.stateMu.Unlock()
	b.notifyStateChange()
	b.logInfo("Backend is now active")

	// Start monitoring for connection loss and auto-reconnect
	go b.monitorConnection()

	return nil
}

// startSSMSession creates a new SSM session
func (b *Backend) startSSMSession() (*SSMSession, error) {
	b.credsMu.RLock()
	creds := b.credentials
	b.credsMu.RUnlock()

	if b.ssmClient == nil {
		return nil, fmt.Errorf("SSM client not configured")
	}

	return StartSSMSession(b.ctx, b.ssmClient, b.config.InstanceID, b.config.Region, creds)
}

// setupDataBridge creates a net.Pipe() bridge between SSH and DataChannel
// This serializes all writes through a single transfer loop, preventing
// parallel writes that overwhelm the SSM agent.
// If setupOutputCallback is true, also sets up the DataChannel output callback.
func (b *Backend) setupDataBridge(setupOutputCallback bool) error {
	// Create in-memory bidirectional pipe
	b.sshConn, b.dcConn = net.Pipe()

	// Start transfer loop for SSH -> DataChannel
	go b.transferDataToSSM()

	// Only set up output callback once (on initial setup, not on retry)
	if setupOutputCallback {
		b.setupDataChannelOutput()
	}

	return nil
}

// setupDataChannelOutput sets up the DataChannel -> SSH callback
// This should only be called once per SSM session
func (b *Backend) setupDataChannelOutput() {
	b.dataChannel.SetOnOutputData(func(data []byte) {
		if b.dcConn == nil {
			// Pipe not ready yet (during cleanup or before setup)
			return
		}
		if _, err := b.dcConn.Write(data); err != nil {
			// Only log if not a normal pipe closure
			select {
			case <-b.ctx.Done():
				// Context cancelled, ignore error
			default:
				logger.Debug("DataChannel output write error", "error", err)
			}
		}
	})
}

// transferDataToSSM reads from dcConn (SSH writes) and sends to DataChannel
// This is the critical serialization point - all SSH writes go through here
func (b *Backend) transferDataToSSM() {
	const chunkSize = 1024
	buf := make([]byte, chunkSize)

	for {
		// Check if we're shutting down
		select {
		case <-b.ctx.Done():
			return
		default:
		}

		// Check if dcConn is still valid
		if b.dcConn == nil {
			return
		}

		n, err := b.dcConn.Read(buf)
		if err != nil {
			// Pipe closed or context cancelled - normal during shutdown
			return
		}

		// Check if dataChannel is still valid
		if b.dataChannel == nil {
			return
		}

		if err := b.dataChannel.SendInputData(buf[:n]); err != nil {
			// Only log if not shutting down
			select {
			case <-b.ctx.Done():
				// Context cancelled, stop silently
			default:
				logger.Debug("transferDataToSSM error", "error", err)
			}
			return
		}

		// Rate limiting like the official plugin (1ms between sends)
		time.Sleep(time.Millisecond)
	}
}


// cleanupDataBridge closes the net.Pipe connections
func (b *Backend) cleanupDataBridge() {
	if b.sshConn != nil {
		b.sshConn.Close()
		b.sshConn = nil
	}
	if b.dcConn != nil {
		b.dcConn.Close()
		b.dcConn = nil
	}
}

// connectSSH establishes SSH over the net.Pipe bridge
// It retries SSH handshake with short intervals since SSM agent may not be ready immediately
func (b *Backend) connectSSH() error {
	if b.sshConfig == nil {
		return fmt.Errorf("SSH config not initialized")
	}

	const maxSSHRetries = 10
	const sshRetryInterval = 500 * time.Millisecond

	var lastErr error
	for attempt := 1; attempt <= maxSSHRetries; attempt++ {
		// Create SSH connection over sshConn side of net.Pipe
		// All writes go through transferDataToSSM for serialization
		sshClientConn, chans, reqs, err := ssh.NewClientConn(b.sshConn, "ssm:22", b.sshConfig)
		if err == nil {
			client := ssh.NewClient(sshClientConn, chans, reqs)
			b.sshMu.Lock()
			b.sshClient = client
			b.sshMu.Unlock()
			if attempt > 1 {
				b.logDebug("SSH handshake succeeded on attempt %d", attempt)
			}
			return nil
		}

		lastErr = err
		// Check if this is a retryable error (overflow = SSM agent not ready yet)
		if attempt < maxSSHRetries {
			b.logDebug("SSH handshake attempt %d failed: %v, retrying...", attempt, err)
			time.Sleep(sshRetryInterval)

			// Recreate the pipe for retry since SSH handshake corrupts the connection
			// Pass false to not re-register the output callback (it's already set)
			b.cleanupDataBridge()
			if err := b.setupDataBridge(false); err != nil {
				return fmt.Errorf("failed to recreate data bridge: %w", err)
			}
		}
	}

	return fmt.Errorf("SSH handshake failed after %d attempts: %w", maxSSHRetries, lastErr)
}

// monitorConnection monitors the SSH connection and triggers reconnect on failure
func (b *Backend) monitorConnection() {
	b.sshMu.RLock()
	client := b.sshClient
	b.sshMu.RUnlock()

	if client == nil {
		return
	}

	// Wait for the SSH connection to close
	// This blocks until the underlying connection is closed
	err := client.Wait()

	// Check if we're still supposed to be running
	select {
	case <-b.ctx.Done():
		// Context cancelled, don't reconnect
		b.logDebug("SSH connection closed (context cancelled)")
		return
	default:
	}

	// Connection lost - trigger reconnect
	b.logWarn("SSH connection lost, will reconnect... error=%v", err)
	b.triggerReconnect()
}

// triggerReconnect initiates a reconnection
func (b *Backend) triggerReconnect() {
	b.stateMu.Lock()
	if b.state == StateReconnecting || b.state == StateConnecting {
		b.logDebug("Already reconnecting, skipping state=%s", b.state.String())
		b.stateMu.Unlock()
		return
	}
	b.state = StateReconnecting
	b.stateMu.Unlock()
	b.notifyStateChange()

	b.logDebug("Cleaning up old connection...")

	// Cleanup old connection
	b.cleanup()

	// Note: Do NOT clear failed hosts cache on reconnect
	// The "No route to host" errors cause SSM to disconnect,
	// so we need to keep blocking those hosts to prevent reconnect loops

	// Reconnect
	b.logInfo("Starting reconnection...")
	b.stateMu.Lock()
	b.state = StateConnecting
	b.stateMu.Unlock()
	b.notifyStateChange()

	go b.connect()
}

// cleanup closes all resources
func (b *Backend) cleanup() {
	b.sshMu.Lock()
	if b.sshClient != nil {
		b.sshClient.Close()
		b.sshClient = nil
	}
	b.sshMu.Unlock()

	b.cleanupDataBridge()

	if b.dataChannel != nil {
		b.dataChannel.Close()
		b.dataChannel = nil
	}
}

// setErrorState sets the backend state to error
func (b *Backend) setErrorState() {
	b.stateMu.Lock()
	b.state = StateError
	b.stateMu.Unlock()
	b.notifyStateChange()
}

// notifyStateChange signals that the state has changed
func (b *Backend) notifyStateChange() {
	select {
	case b.stateChange <- struct{}{}:
	default:
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

// State returns the current state (for testing)
func (b *Backend) State() State {
	b.stateMu.RLock()
	defer b.stateMu.RUnlock()
	return b.state
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
