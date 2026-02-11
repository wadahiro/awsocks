// Package agent implements the VM-side agent that handles connections and SSM sessions
package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/wadahiro/awsocks/internal/backend"
	ssmbackend "github.com/wadahiro/awsocks/internal/backend/ssm"
	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/protocol"
)

var logger = log.For(log.ComponentAgent)

// Agent handles communication with the host and manages SSM connections
type Agent struct {
	conn          net.Conn
	connections   map[uint32]*Connection
	connMu        sync.RWMutex
	connWriteMu   sync.Mutex // protects writes to conn
	credentials   *CredentialCache
	backend       backend.Backend
	backendConfig *protocol.BackendConfigPayload // Backend config received from host
	ctx           context.Context
	cancel        context.CancelFunc
}

// Connection represents an active connection being proxied
type Connection struct {
	ID       uint32
	conn     net.Conn
	agent    *Agent
	ctx      context.Context
	cancel   context.CancelFunc
	closeMu  sync.Once
}

// New creates a new agent instance
func New(vsockConn net.Conn, b backend.Backend) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	return &Agent{
		conn:        vsockConn,
		connections: make(map[uint32]*Connection),
		credentials: NewCredentialCache(),
		backend:     b,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// NewWithoutBackend creates a new agent instance without a backend (for direct connections)
func NewWithoutBackend(vsockConn net.Conn) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	return &Agent{
		conn:        vsockConn,
		connections: make(map[uint32]*Connection),
		credentials: NewCredentialCache(),
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Run starts the agent main loop
func (a *Agent) Run() error {
	logger.Info("Starting main loop...")

	for {
		msg, err := protocol.ReadMessage(a.conn)
		if err != nil {
			if err == io.EOF {
				logger.Info("Host disconnected")
				return nil
			}
			return fmt.Errorf("failed to read message: %w", err)
		}

		if err := a.handleMessage(msg); err != nil {
			logger.Error("Error handling message", "error", err)
		}
	}
}

func (a *Agent) handleMessage(msg *protocol.Message) error {
	switch msg.Type {
	case protocol.MsgBackendConfig:
		return a.handleBackendConfig(msg)
	case protocol.MsgConnect:
		return a.handleConnect(msg)
	case protocol.MsgConnectDirect:
		return a.handleConnectDirect(msg)
	case protocol.MsgData:
		return a.handleData(msg)
	case protocol.MsgClose:
		return a.handleClose(msg)
	case protocol.MsgCredentialUpdate:
		return a.handleCredentialUpdate(msg)
	case protocol.MsgPing:
		return a.handlePing(msg)
	case protocol.MsgShutdown:
		logger.Info("Received shutdown request")
		a.cancel()
		return nil
	default:
		logger.Warn("Unknown message type", "type", msg.Type)
		return nil
	}
}

func (a *Agent) handleConnect(msg *protocol.Message) error {
	network, address, err := protocol.ParseConnectPayload(msg.Payload)
	if err != nil {
		a.sendError(msg.ConnID, 1, err.Error())
		return err
	}

	logger.Debug("Connect request", "network", network, "address", address, "connID", msg.ConnID)

	// Handle connection asynchronously to avoid blocking the message loop
	go a.dialAndConnect(msg.ConnID, network, address, false)

	return nil
}

func (a *Agent) handleConnectDirect(msg *protocol.Message) error {
	network, address, err := protocol.ParseConnectPayload(msg.Payload)
	if err != nil {
		a.sendError(msg.ConnID, 1, err.Error())
		return err
	}

	logger.Debug("ConnectDirect request (VM NAT)", "network", network, "address", address, "connID", msg.ConnID)

	// Handle connection asynchronously with direct flag
	go a.dialAndConnect(msg.ConnID, network, address, true)

	return nil
}

func (a *Agent) dialAndConnect(connID uint32, network, address string, directMode bool) {
	a.logDebug("dialAndConnect connID=%d address=%s directMode=%v hasBackend=%v", connID, address, directMode, a.backend != nil)

	// Dial the target
	// Use longer timeout to allow for SSM connection establishment (can take 30-60s)
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	defer cancel()

	var conn net.Conn
	var err error
	if directMode {
		// Direct connection via VM's NAT (bypass EC2 backend)
		a.logDebug("Using direct connection (VM NAT)")
		var d net.Dialer
		conn, err = d.DialContext(ctx, network, address)
	} else if a.backend != nil {
		// Use backend for connection (via EC2)
		a.logDebug("Using backend for connection")
		conn, err = a.backend.Dial(ctx, network, address)
	} else {
		// No backend configured, fall back to direct connection
		a.logDebug("No backend, falling back to direct connection")
		var d net.Dialer
		conn, err = d.DialContext(ctx, network, address)
	}
	if err != nil {
		a.sendError(connID, 2, err.Error())
		logger.Debug("Failed to dial", "address", address, "connID", connID, "error", err)
		return
	}

	// Create connection wrapper
	connCtx, connCancel := context.WithCancel(a.ctx)
	c := &Connection{
		ID:     connID,
		conn:   conn,
		agent:  a,
		ctx:    connCtx,
		cancel: connCancel,
	}

	// Store connection
	a.connMu.Lock()
	a.connections[connID] = c
	a.connMu.Unlock()

	// Send ack
	ack := &protocol.Message{
		Type:   protocol.MsgConnectAck,
		ConnID: connID,
	}
	if err := protocol.WriteMessage(a.conn, ack); err != nil {
		logger.Error("Failed to send connect ack", "connID", connID, "error", err)
		conn.Close()
		return
	}

	// Start reading from target
	go c.readLoop()
}

func (a *Agent) handleData(msg *protocol.Message) error {
	a.connMu.RLock()
	c, ok := a.connections[msg.ConnID]
	a.connMu.RUnlock()

	if !ok {
		return fmt.Errorf("unknown connection: %d", msg.ConnID)
	}

	_, err := c.conn.Write(msg.Payload)
	return err
}

func (a *Agent) handleClose(msg *protocol.Message) error {
	a.connMu.Lock()
	c, ok := a.connections[msg.ConnID]
	if ok {
		delete(a.connections, msg.ConnID)
	}
	a.connMu.Unlock()

	if ok {
		c.Close()
	}

	return nil
}

func (a *Agent) handleBackendConfig(msg *protocol.Message) error {
	a.logInfo("Received backend config")

	cfg, err := protocol.ParseBackendConfigPayload(msg.Payload)
	if err != nil {
		return fmt.Errorf("failed to parse backend config: %w", err)
	}

	a.backendConfig = cfg
	a.logInfo("Backend config: type=%s instance=%s region=%s user=%s lazy=%v", cfg.Type, cfg.InstanceID, cfg.Region, cfg.SSHUser, cfg.LazyConnect)

	// If backend already exists and instance ID is now resolved, update the backend config
	if a.backend != nil && cfg.InstanceID != "" {
		if ssmBackend, ok := a.backend.(*ssmbackend.Backend); ok {
			ssmBackend.UpdateInstanceID(cfg.InstanceID)
			a.logInfo("Updated backend instance ID: %s", cfg.InstanceID)
		}
	}

	// Backend will be initialized when credentials are received
	return nil
}

func (a *Agent) handleCredentialUpdate(msg *protocol.Message) error {
	a.logInfo("Received credential update")
	// Parse credentials from payload
	// Format: AccessKeyID\nSecretAccessKey\nSessionToken\nExpiration
	creds, err := parseCredentials(msg.Payload)
	if err != nil {
		return fmt.Errorf("failed to parse credentials: %w", err)
	}

	a.credentials.Update(creds)

	// Initialize backend if config is set but backend is not yet created
	if a.backendConfig != nil && a.backend == nil {
		a.logInfo("Initializing backend from credential update...")
		if err := a.initializeBackend(creds); err != nil {
			a.logError("Failed to initialize backend: %v", err)
			return fmt.Errorf("failed to initialize backend: %w", err)
		}
	}

	// Notify backend of credential update
	if a.backend != nil {
		// In lazy mode, just store credentials without triggering connection
		if a.backendConfig != nil && a.backendConfig.LazyConnect {
			a.logInfo("Lazy mode: storing credentials without connecting")
			if setter, ok := a.backend.(interface{ SetCredentials(aws.Credentials) }); ok {
				setter.SetCredentials(creds)
				a.logInfo("Credentials stored (lazy mode)")
			} else {
				a.logError("Backend does not support SetCredentials")
			}
		} else {
			// Immediate mode: trigger connection
			a.logInfo("Immediate mode: triggering connection")
			if err := a.backend.OnCredentialUpdate(creds); err != nil {
				a.logError("Backend credential update failed: %v", err)
			}
		}
	}

	a.logInfo("Credentials updated successfully")
	return nil
}

func (a *Agent) initializeBackend(creds aws.Credentials) error {
	cfg := a.backendConfig
	if cfg.Type != "muxssh" && cfg.Type != "sshproxy" {
		return fmt.Errorf("unsupported backend type: %s", cfg.Type)
	}

	a.logInfo("Initializing MuxSSH backend...")

	// Create minimal AWS config with static credentials (no LoadDefaultConfig)
	awsCfg := aws.Config{
		Region: cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(
			creds.AccessKeyID,
			creds.SecretAccessKey,
			creds.SessionToken,
		),
	}

	// Create SSM client
	ssmClient := ssmbackend.NewHTTPClient(awsCfg)

	// Create MuxSSH backend with log callback to forward logs to host
	b := ssmbackend.New(&ssmbackend.Config{
		InstanceID: cfg.InstanceID,
		Region:     cfg.Region,
		SSHUser:    cfg.SSHUser,
		LogFunc:    a.sendLog,
	}, ssmClient)

	// Set SSH key from content
	if err := b.SetSSHKeyContent(cfg.SSHKeyContent, cfg.SSHKeyPassphrase); err != nil {
		a.logError("Failed to set SSH key: %v", err)
		return fmt.Errorf("failed to set SSH key: %w", err)
	}

	// Start backend
	if err := b.Start(a.ctx); err != nil {
		a.logError("Failed to start backend: %v", err)
		return fmt.Errorf("failed to start backend: %w", err)
	}

	a.backend = b
	a.logInfo("MuxSSH backend initialized")
	return nil
}

func (a *Agent) handlePing(msg *protocol.Message) error {
	pong := &protocol.Message{
		Type: protocol.MsgPong,
	}
	return protocol.WriteMessage(a.conn, pong)
}

func (a *Agent) sendError(connID uint32, code int, message string) {
	msg := protocol.NewErrorMessage(connID, code, message)
	if err := protocol.WriteMessage(a.conn, msg); err != nil {
		logger.Error("Failed to send error message", "error", err)
	}
}

func (a *Agent) sendData(connID uint32, data []byte) error {
	msg := protocol.NewDataMessage(connID, data)
	return protocol.WriteMessage(a.conn, msg)
}

func (a *Agent) sendClose(connID uint32) error {
	msg := protocol.NewCloseMessage(connID)
	return a.writeMessage(msg)
}

// writeMessage safely writes a message to the host connection
func (a *Agent) writeMessage(msg *protocol.Message) error {
	a.connWriteMu.Lock()
	defer a.connWriteMu.Unlock()
	return protocol.WriteMessage(a.conn, msg)
}

// sendLog sends a log message to the host for display
func (a *Agent) sendLog(level, format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	msg := protocol.NewLogMessage(level, message)
	a.writeMessage(msg)
}

// logInfo sends an info log to the host
func (a *Agent) logInfo(format string, args ...interface{}) {
	a.sendLog("info", format, args...)
}

// logDebug sends a debug log to the host
func (a *Agent) logDebug(format string, args ...interface{}) {
	a.sendLog("debug", format, args...)
}

// logError sends an error log to the host
func (a *Agent) logError(format string, args ...interface{}) {
	a.sendLog("error", format, args...)
}

// Connection methods

func (c *Connection) readLoop() {
	defer c.Close()

	buf := make([]byte, 32*1024)
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.conn.SetReadDeadline(time.Now().Add(time.Minute))
		n, err := c.conn.Read(buf)
		if err != nil {
			if err != io.EOF {
				logger.Debug("Read error", "connID", c.ID, "error", err)
			}
			return
		}

		if err := c.agent.sendData(c.ID, buf[:n]); err != nil {
			logger.Debug("Failed to send data", "connID", c.ID, "error", err)
			return
		}
	}
}

func (c *Connection) Close() {
	c.closeMu.Do(func() {
		c.cancel()
		c.conn.Close()
		c.agent.sendClose(c.ID)
	})
}

// Credential helper functions

func parseCredentials(payload []byte) (aws.Credentials, error) {
	s := string(payload)
	var parts []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])

	if len(parts) < 4 {
		return aws.Credentials{}, fmt.Errorf("invalid credential format")
	}

	var expiration time.Time
	if parts[3] != "" && parts[3] != "0" {
		var unix int64
		fmt.Sscanf(parts[3], "%d", &unix)
		if unix > 0 {
			expiration = time.Unix(unix, 0)
		}
	}

	return aws.Credentials{
		AccessKeyID:     parts[0],
		SecretAccessKey: parts[1],
		SessionToken:    parts[2],
		Expires:         expiration,
	}, nil
}
