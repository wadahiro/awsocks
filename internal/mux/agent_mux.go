// Package mux provides a shared multiplexer for agent connections over vsock.
// It owns the underlying net.Conn and provides a single read/write loop,
// preventing the dual-reader bug where multiple goroutines compete to read
// from the same connection.
package mux

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/protocol"
)

var logger = log.For(log.ComponentProxy)

// Option configures AgentMux behavior.
type Option func(*AgentMux)

// WithLogHandler sets a callback for MsgLog messages from the agent.
func WithLogHandler(fn func(payload *protocol.LogPayload)) Option {
	return func(m *AgentMux) {
		m.onLog = fn
	}
}

// AgentMux multiplexes multiple logical connections over a single agent connection.
// It is the sole owner of the underlying net.Conn and runs a single read loop.
type AgentMux struct {
	conn       net.Conn
	writeMu    sync.Mutex
	nextConnID uint32
	pending    map[uint32]chan connResult
	pendingMu  sync.Mutex
	conns      map[uint32]*MuxConn
	connsMu    sync.RWMutex
	onLog      func(payload *protocol.LogPayload)
	ctx        context.Context
	cancel     context.CancelFunc
}

// connResult holds the result of a Dial attempt.
type connResult struct {
	conn net.Conn
	err  error
}

// NewAgentMux creates a new multiplexer that owns the given connection.
// It immediately starts the read loop goroutine.
func NewAgentMux(conn net.Conn, opts ...Option) *AgentMux {
	ctx, cancel := context.WithCancel(context.Background())
	m := &AgentMux{
		conn:    conn,
		pending: make(map[uint32]chan connResult),
		conns:   make(map[uint32]*MuxConn),
		ctx:     ctx,
		cancel:  cancel,
	}
	for _, opt := range opts {
		opt(m)
	}
	go m.readLoop()
	return m
}

// Dial creates a new logical connection to the given address via the agent.
func (m *AgentMux) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	select {
	case <-m.ctx.Done():
		return nil, fmt.Errorf("agent mux closed")
	default:
	}

	connID := atomic.AddUint32(&m.nextConnID, 1)

	respCh := make(chan connResult, 1)
	m.pendingMu.Lock()
	m.pending[connID] = respCh
	m.pendingMu.Unlock()

	defer func() {
		m.pendingMu.Lock()
		delete(m.pending, connID)
		m.pendingMu.Unlock()
	}()

	msg := protocol.NewConnectDirectMessage(connID, network, addr)
	m.writeMu.Lock()
	err := protocol.WriteMessage(m.conn, msg)
	m.writeMu.Unlock()

	if err != nil {
		return nil, fmt.Errorf("failed to send connect request: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.ctx.Done():
		return nil, fmt.Errorf("agent mux closed")
	case result := <-respCh:
		if result.err != nil {
			return nil, result.err
		}
		return result.conn, nil
	}
}

// SendShutdown sends a shutdown message to the agent.
func (m *AgentMux) SendShutdown() error {
	msg := &protocol.Message{Type: protocol.MsgShutdown}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	return protocol.WriteMessage(m.conn, msg)
}

// Close stops the read loop and closes the underlying connection.
func (m *AgentMux) Close() error {
	m.cancel()
	return m.conn.Close()
}

// readLoop is the single goroutine that reads all messages from the agent.
func (m *AgentMux) readLoop() {
	for {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		msg, err := protocol.ReadMessage(m.conn)
		if err != nil {
			if err != io.EOF {
				select {
				case <-m.ctx.Done():
					// Normal shutdown, don't log
				default:
					logger.Error("Error reading from agent", "error", err)
				}
			}
			// Fail all pending dials
			m.pendingMu.Lock()
			for _, ch := range m.pending {
				select {
				case ch <- connResult{err: fmt.Errorf("agent connection closed")}:
				default:
				}
			}
			m.pendingMu.Unlock()
			return
		}

		switch msg.Type {
		case protocol.MsgConnectAck:
			m.handleConnectAck(msg)
		case protocol.MsgData:
			m.handleData(msg)
		case protocol.MsgClose:
			m.handleClose(msg)
		case protocol.MsgError:
			m.handleError(msg)
		case protocol.MsgLog:
			m.handleLog(msg)
		case protocol.MsgPong:
			// Ignore pong
		default:
			logger.Warn("Unknown message type", "type", msg.Type)
		}
	}
}

func (m *AgentMux) handleConnectAck(msg *protocol.Message) {
	m.pendingMu.Lock()
	respCh, ok := m.pending[msg.ConnID]
	m.pendingMu.Unlock()

	if !ok {
		logger.Debug("ConnectAck for timed-out connection", "connID", msg.ConnID)
		return
	}

	conn := newMuxConn(msg.ConnID, m)
	m.connsMu.Lock()
	m.conns[msg.ConnID] = conn
	m.connsMu.Unlock()

	respCh <- connResult{conn: conn}
}

func (m *AgentMux) handleData(msg *protocol.Message) {
	m.connsMu.RLock()
	conn, ok := m.conns[msg.ConnID]
	m.connsMu.RUnlock()

	if !ok {
		return
	}

	dataCopy := make([]byte, len(msg.Payload))
	copy(dataCopy, msg.Payload)

	conn.mu.Lock()
	closed := conn.closed
	conn.mu.Unlock()

	if !closed {
		select {
		case conn.readBuf <- dataCopy:
		default:
			logger.Warn("Read buffer full for connection", "connID", msg.ConnID)
		}
	}
}

func (m *AgentMux) handleClose(msg *protocol.Message) {
	m.connsMu.RLock()
	conn, ok := m.conns[msg.ConnID]
	m.connsMu.RUnlock()

	if ok {
		conn.closeFromRemote()
	}
}

func (m *AgentMux) handleError(msg *protocol.Message) {
	errMsg := string(msg.Payload)

	m.pendingMu.Lock()
	respCh, ok := m.pending[msg.ConnID]
	m.pendingMu.Unlock()

	if ok {
		respCh <- connResult{err: fmt.Errorf("%s", errMsg)}
	}

	// Also close the connection if it exists
	m.connsMu.RLock()
	conn, connOk := m.conns[msg.ConnID]
	m.connsMu.RUnlock()

	if connOk {
		conn.closeFromRemote()
	}
}

func (m *AgentMux) handleLog(msg *protocol.Message) {
	if m.onLog == nil {
		return
	}

	logPayload, err := protocol.ParseLogPayload(msg.Payload)
	if err != nil {
		logger.Warn("Failed to parse log message", "error", err)
		return
	}

	m.onLog(logPayload)
}

// writeMessage sends a protocol message through the shared connection.
func (m *AgentMux) writeMessage(msg *protocol.Message) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	return protocol.WriteMessage(m.conn, msg)
}

// unregisterConn removes a connection from the active map.
func (m *AgentMux) unregisterConn(id uint32) {
	m.connsMu.Lock()
	delete(m.conns, id)
	m.connsMu.Unlock()
}

// MuxConn implements net.Conn for a logical connection multiplexed over AgentMux.
type MuxConn struct {
	id        uint32
	mux       *AgentMux
	readBuf   chan []byte
	remaining []byte
	closed    bool
	mu        sync.Mutex
	closeOnce sync.Once
}

func newMuxConn(id uint32, mux *AgentMux) *MuxConn {
	return &MuxConn{
		id:      id,
		mux:     mux,
		readBuf: make(chan []byte, 256),
	}
}

func (c *MuxConn) Read(b []byte) (int, error) {
	// Drain remaining data from previous partial read
	if len(c.remaining) > 0 {
		n := copy(b, c.remaining)
		c.remaining = c.remaining[n:]
		return n, nil
	}

	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()

	if closed {
		return 0, io.EOF
	}

	select {
	case data, ok := <-c.readBuf:
		if !ok {
			return 0, io.EOF
		}
		n := copy(b, data)
		if n < len(data) {
			c.remaining = data[n:]
		}
		return n, nil
	case <-time.After(time.Minute):
		return 0, fmt.Errorf("read timeout")
	}
}

func (c *MuxConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()

	if closed {
		return 0, io.ErrClosedPipe
	}

	msg := protocol.NewDataMessage(c.id, b)
	if err := c.mux.writeMessage(msg); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *MuxConn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		close(c.readBuf)
		c.mu.Unlock()

		c.mux.unregisterConn(c.id)

		msg := protocol.NewCloseMessage(c.id)
		c.mux.writeMessage(msg)
	})
	return nil
}

// closeFromRemote is called when the agent sends MsgClose.
func (c *MuxConn) closeFromRemote() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		close(c.readBuf)
		c.mu.Unlock()

		c.mux.unregisterConn(c.id)
	})
}

func (c *MuxConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
}

func (c *MuxConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 0}
}

func (c *MuxConn) SetDeadline(t time.Time) error      { return nil }
func (c *MuxConn) SetReadDeadline(t time.Time) error   { return nil }
func (c *MuxConn) SetWriteDeadline(t time.Time) error  { return nil }
