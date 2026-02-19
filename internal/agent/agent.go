// Package agent implements the VM-side agent that handles direct connections via NAT
package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/protocol"
)

var logger = log.For(log.ComponentAgent)

// Agent handles communication with the host and manages direct connections via VM NAT.
// The agent only handles MsgConnectDirect for TCP proxy - all SSM/proxy logic is on the host side.
type Agent struct {
	conn        net.Conn
	connections map[uint32]*Connection
	connMu      sync.RWMutex
	connWriteMu sync.Mutex // protects writes to conn
	ctx         context.Context
	cancel      context.CancelFunc
}

// Connection represents an active connection being proxied
type Connection struct {
	ID      uint32
	conn    net.Conn
	agent   *Agent
	ctx     context.Context
	cancel  context.CancelFunc
	closeMu sync.Once
}

// New creates a new agent instance
func New(vsockConn net.Conn) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	return &Agent{
		conn:        vsockConn,
		connections: make(map[uint32]*Connection),
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
	case protocol.MsgConnectDirect:
		return a.handleConnectDirect(msg)
	case protocol.MsgData:
		return a.handleData(msg)
	case protocol.MsgClose:
		return a.handleClose(msg)
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

func (a *Agent) handleConnectDirect(msg *protocol.Message) error {
	network, address, err := protocol.ParseConnectPayload(msg.Payload)
	if err != nil {
		a.sendError(msg.ConnID, 1, err.Error())
		return err
	}

	logger.Debug("ConnectDirect request (VM NAT)", "network", network, "address", address, "connID", msg.ConnID)

	go a.dialAndConnect(msg.ConnID, network, address)

	return nil
}

func (a *Agent) dialAndConnect(connID uint32, network, address string) {
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, network, address)
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
