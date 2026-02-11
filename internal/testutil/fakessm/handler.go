package fakessm

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/wadahiro/awsocks/internal/backend/ssm/datachannel"
)

// Session represents a single SSM session connection
type Session struct {
	ID            string
	conn          *websocket.Conn
	server        *Server
	mu            sync.Mutex
	writeMu       sync.Mutex
	handshakeDone bool
	sendSeqNum    int64
	recvSeqNum    int64
	closed        bool

	// Echo mode: if true, received data will be echoed back
	EchoMode bool

	// SSH mode: connection to SSH server for forwarding
	sshConn net.Conn

	// Data received from client (for assertions)
	ReceivedData [][]byte
}

// NewSession creates a new Session
func NewSession(id string, conn *websocket.Conn, server *Server) *Session {
	s := &Session{
		ID:           id,
		conn:         conn,
		server:       server,
		EchoMode:     true,
		ReceivedData: make([][]byte, 0),
	}

	return s
}

// Run starts the session message loop
func (s *Session) Run() {
	defer func() {
		s.Close()
		s.server.removeSession(s.ID)
	}()

	// Wait for OpenDataChannel JSON message
	for {
		messageType, data, err := s.conn.ReadMessage()
		if err != nil {
			if s.server.opts.OnDisconnect != nil {
				s.server.opts.OnDisconnect(s.ID)
			}
			return
		}

		if messageType == websocket.TextMessage {
			// JSON message - OpenDataChannel
			if err := s.handleOpenDataChannel(data); err != nil {
				fmt.Printf("[fakessm] handleOpenDataChannel error: %v\n", err)
				return
			}
			break
		}
	}

	// Start handshake
	if err := s.startHandshake(); err != nil {
		fmt.Printf("[fakessm] startHandshake error: %v\n", err)
		return
	}

	// Main message loop
	for {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()

		if closed {
			return
		}

		messageType, data, err := s.conn.ReadMessage()
		if err != nil {
			if s.server.opts.OnDisconnect != nil {
				s.server.opts.OnDisconnect(s.ID)
			}
			return
		}

		if messageType == websocket.BinaryMessage {
			if err := s.handleBinaryMessage(data); err != nil {
				fmt.Printf("[fakessm] handleBinaryMessage error: %v\n", err)
				return
			}
		}
	}
}

// handleOpenDataChannel processes the initial OpenDataChannel JSON message
func (s *Session) handleOpenDataChannel(data []byte) error {
	var input OpenDataChannelInput
	if err := json.Unmarshal(data, &input); err != nil {
		return fmt.Errorf("failed to unmarshal OpenDataChannel: %w", err)
	}

	if s.server.opts.OnConnect != nil {
		s.server.opts.OnConnect(s.ID)
	}

	return nil
}

// startHandshake sends the handshake request to the client
func (s *Session) startHandshake() error {
	// Simulate delay if configured
	if s.server.opts.SimulateDelay > 0 {
		time.Sleep(s.server.opts.SimulateDelay)
	}

	// Send HandshakeRequest
	msg, err := NewHandshakeRequestMessage(s.server.opts.AgentVersion, "Port", s.sendSeqNum)
	if err != nil {
		return fmt.Errorf("failed to create handshake request: %w", err)
	}
	s.sendSeqNum++

	if err := s.sendMessage(msg); err != nil {
		return fmt.Errorf("failed to send handshake request: %w", err)
	}

	return nil
}

// handleBinaryMessage processes incoming binary messages
func (s *Session) handleBinaryMessage(data []byte) error {
	if s.server.opts.OnMessage != nil {
		s.server.opts.OnMessage(s.ID, data)
	}

	msg := &datachannel.ClientMessage{}
	if err := msg.Deserialize(data); err != nil {
		return fmt.Errorf("failed to deserialize message: %w", err)
	}

	switch msg.MessageType {
	case datachannel.InputStreamMessage:
		return s.handleInputStreamMessage(msg)
	case datachannel.AcknowledgeMessage:
		// ACK received from client - nothing to do
		return nil
	default:
		// Unknown message type - ignore
		return nil
	}
}

// handleInputStreamMessage handles input stream messages from the client
func (s *Session) handleInputStreamMessage(msg *datachannel.ClientMessage) error {
	// Always send ACK first
	if err := s.sendAck(msg.MessageType, msg.MessageID, msg.SequenceNumber); err != nil {
		return fmt.Errorf("failed to send ACK: %w", err)
	}

	switch msg.PayloadType {
	case datachannel.PayloadTypeHandshakeResponse:
		return s.handleHandshakeResponse(msg)
	case datachannel.PayloadTypeOutput:
		return s.handleOutputData(msg)
	default:
		// Unknown payload type - ignore
		return nil
	}
}

// handleHandshakeResponse handles handshake response from client
func (s *Session) handleHandshakeResponse(msg *datachannel.ClientMessage) error {
	// Parse and validate the response
	resp := &datachannel.HandshakeResponse{}
	if err := json.Unmarshal(msg.Payload, resp); err != nil {
		return fmt.Errorf("failed to parse handshake response: %w", err)
	}

	// Send HandshakeComplete
	completeMsg, err := NewHandshakeCompleteMessage(s.sendSeqNum)
	if err != nil {
		return fmt.Errorf("failed to create handshake complete: %w", err)
	}
	s.sendSeqNum++

	if err := s.sendMessage(completeMsg); err != nil {
		return fmt.Errorf("failed to send handshake complete: %w", err)
	}

	s.mu.Lock()
	s.handshakeDone = true
	s.mu.Unlock()

	// After SSM handshake is complete, connect to SSH server if configured
	if s.server.opts.SSHServerAddr != "" {
		sshConn, err := net.Dial("tcp", s.server.opts.SSHServerAddr)
		if err != nil {
			return fmt.Errorf("failed to connect to SSH server: %w", err)
		}
		s.mu.Lock()
		s.sshConn = sshConn
		s.EchoMode = false
		s.mu.Unlock()

		// Start forwarding from SSH server to client
		go s.forwardFromSSH()
	}

	return nil
}

// handleOutputData handles output data from client (input from our perspective)
func (s *Session) handleOutputData(msg *datachannel.ClientMessage) error {
	// Store received data
	s.mu.Lock()
	s.ReceivedData = append(s.ReceivedData, msg.Payload)
	echoMode := s.EchoMode
	sshConn := s.sshConn
	s.mu.Unlock()

	// Forward to SSH server if connected
	if sshConn != nil {
		_, err := sshConn.Write(msg.Payload)
		return err
	}

	// Echo back if configured
	if echoMode {
		return s.SendData(msg.Payload)
	}

	return nil
}

// forwardFromSSH reads data from SSH server and sends it to the client
func (s *Session) forwardFromSSH() {
	buf := make([]byte, 32*1024)
	for {
		s.mu.Lock()
		sshConn := s.sshConn
		closed := s.closed
		s.mu.Unlock()

		if sshConn == nil || closed {
			return
		}

		n, err := sshConn.Read(buf)
		if err != nil {
			return
		}

		if err := s.SendData(buf[:n]); err != nil {
			return
		}
	}
}

// SendData sends data to the client
func (s *Session) SendData(payload []byte) error {
	s.mu.Lock()
	seqNum := s.sendSeqNum
	s.sendSeqNum++
	s.mu.Unlock()

	msg := NewOutputDataMessage(payload, seqNum)
	return s.sendMessage(msg)
}

// sendMessage serializes and sends a message to the client
func (s *Session) sendMessage(msg *datachannel.ClientMessage) error {
	data, err := msg.Serialize()
	if err != nil {
		return fmt.Errorf("failed to serialize message: %w", err)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	return s.conn.WriteMessage(websocket.BinaryMessage, data)
}

// sendAck sends an ACK message to the client
func (s *Session) sendAck(messageType string, messageID uuid.UUID, seqNum int64) error {
	ack, err := NewAcknowledgeMessage(messageType, messageID, seqNum)
	if err != nil {
		return err
	}
	return s.sendMessage(ack)
}

// Close closes the session
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	// Close SSH connection if present
	if s.sshConn != nil {
		s.sshConn.Close()
	}

	return s.conn.Close()
}

// IsHandshakeComplete returns whether the handshake has completed
func (s *Session) IsHandshakeComplete() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handshakeDone
}

// GetReceivedData returns a copy of all received data
func (s *Session) GetReceivedData() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([][]byte, len(s.ReceivedData))
	for i, d := range s.ReceivedData {
		result[i] = make([]byte, len(d))
		copy(result[i], d)
	}
	return result
}
