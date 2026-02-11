// Package protocol defines the communication protocol between Mac host and VM agent over vsock
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// MessageType defines the type of message
type MessageType byte

const (
	// Connection management
	MsgConnect       MessageType = 0x01
	MsgConnectAck    MessageType = 0x02
	MsgData          MessageType = 0x03
	MsgClose         MessageType = 0x04
	MsgError         MessageType = 0x05
	MsgConnectDirect MessageType = 0x06 // VM NAT direct connection (bypass EC2)

	// Credential management
	MsgCredentialUpdate MessageType = 0x10

	// Control messages
	MsgPing     MessageType = 0x20
	MsgPong     MessageType = 0x21
	MsgShutdown MessageType = 0x22

	// Backend configuration
	MsgBackendConfig MessageType = 0x30

	// Agent log forwarding
	MsgLog MessageType = 0x40
)

// Message represents a protocol message
type Message struct {
	Type    MessageType
	ConnID  uint32
	Payload []byte
}

// ConnectPayload contains connection request details
type ConnectPayload struct {
	Network string // "tcp" or "udp"
	Address string // host:port
}

// CredentialPayload contains AWS credentials
type CredentialPayload struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// ErrorPayload contains error details
type ErrorPayload struct {
	Code    int
	Message string
}

// BackendConfigPayload contains backend configuration
type BackendConfigPayload struct {
	Type             string `json:"type"`             // "muxssh"
	InstanceID       string `json:"instanceId"`
	Region           string `json:"region"`
	SSHUser          string `json:"sshUser"`
	SSHKeyContent    []byte `json:"sshKeyContent"`    // SSH private key bytes
	SSHKeyPassphrase string `json:"sshKeyPassphrase"` // Optional passphrase
	LazyConnect      bool   `json:"lazyConnect"`      // Lazy connection mode
}

// Encode serializes a message for transmission
func (m *Message) Encode() ([]byte, error) {
	// Format: [type:1][connID:4][payloadLen:4][payload:N]
	payloadLen := len(m.Payload)
	buf := make([]byte, 9+payloadLen)

	buf[0] = byte(m.Type)
	binary.BigEndian.PutUint32(buf[1:5], m.ConnID)
	binary.BigEndian.PutUint32(buf[5:9], uint32(payloadLen))

	if payloadLen > 0 {
		copy(buf[9:], m.Payload)
	}

	return buf, nil
}

// ReadMessage reads and decodes a message from a reader
func ReadMessage(r io.Reader) (*Message, error) {
	header := make([]byte, 9)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, fmt.Errorf("failed to read header: %w", err)
	}

	msgType := MessageType(header[0])
	connID := binary.BigEndian.Uint32(header[1:5])
	payloadLen := binary.BigEndian.Uint32(header[5:9])

	// Sanity check payload length (max 10MB)
	if payloadLen > 10*1024*1024 {
		return nil, fmt.Errorf("payload too large: %d bytes", payloadLen)
	}

	var payload []byte
	if payloadLen > 0 {
		payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, fmt.Errorf("failed to read payload: %w", err)
		}
	}

	return &Message{
		Type:    msgType,
		ConnID:  connID,
		Payload: payload,
	}, nil
}

// WriteMessage encodes and writes a message to a writer
func WriteMessage(w io.Writer, m *Message) error {
	data, err := m.Encode()
	if err != nil {
		return err
	}

	_, err = w.Write(data)
	return err
}

// NewConnectMessage creates a connection request message
func NewConnectMessage(connID uint32, network, address string) *Message {
	payload := []byte(fmt.Sprintf("%s:%s", network, address))
	return &Message{
		Type:    MsgConnect,
		ConnID:  connID,
		Payload: payload,
	}
}

// NewConnectDirectMessage creates a direct connection request message (VM NAT, bypass EC2)
func NewConnectDirectMessage(connID uint32, network, address string) *Message {
	payload := []byte(fmt.Sprintf("%s:%s", network, address))
	return &Message{
		Type:    MsgConnectDirect,
		ConnID:  connID,
		Payload: payload,
	}
}

// NewDataMessage creates a data transfer message
func NewDataMessage(connID uint32, data []byte) *Message {
	return &Message{
		Type:    MsgData,
		ConnID:  connID,
		Payload: data,
	}
}

// NewCloseMessage creates a connection close message
func NewCloseMessage(connID uint32) *Message {
	return &Message{
		Type:   MsgClose,
		ConnID: connID,
	}
}

// NewErrorMessage creates an error message
func NewErrorMessage(connID uint32, code int, message string) *Message {
	payload := []byte(fmt.Sprintf("%d:%s", code, message))
	return &Message{
		Type:    MsgError,
		ConnID:  connID,
		Payload: payload,
	}
}

// NewCredentialUpdateMessage creates a credential update message
func NewCredentialUpdateMessage(creds CredentialPayload) *Message {
	// Simple encoding: fields separated by newlines
	payload := fmt.Sprintf("%s\n%s\n%s\n%d",
		creds.AccessKeyID,
		creds.SecretAccessKey,
		creds.SessionToken,
		creds.Expiration.Unix(),
	)
	return &Message{
		Type:    MsgCredentialUpdate,
		Payload: []byte(payload),
	}
}

// ParseConnectPayload parses a connect message payload
func ParseConnectPayload(payload []byte) (network, address string, err error) {
	s := string(payload)
	for i, c := range s {
		if c == ':' {
			return s[:i], s[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("invalid connect payload: %s", s)
}

// NewBackendConfigMessage creates a backend configuration message
func NewBackendConfigMessage(cfg *BackendConfigPayload) *Message {
	payload, _ := json.Marshal(cfg)
	return &Message{
		Type:    MsgBackendConfig,
		Payload: payload,
	}
}

// ParseBackendConfigPayload parses a backend config message payload
func ParseBackendConfigPayload(payload []byte) (*BackendConfigPayload, error) {
	var cfg BackendConfigPayload
	if err := json.Unmarshal(payload, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse backend config: %w", err)
	}
	return &cfg, nil
}

// LogPayload contains log message from agent
type LogPayload struct {
	Level   string `json:"level"`   // "debug", "info", "warn", "error"
	Message string `json:"message"`
}

// NewLogMessage creates a log message
func NewLogMessage(level, message string) *Message {
	payload, _ := json.Marshal(LogPayload{Level: level, Message: message})
	return &Message{
		Type:    MsgLog,
		Payload: payload,
	}
}

// ParseLogPayload parses a log message payload
func ParseLogPayload(payload []byte) (*LogPayload, error) {
	var log LogPayload
	if err := json.Unmarshal(payload, &log); err != nil {
		return nil, err
	}
	return &log, nil
}

// String returns a human-readable message type name
func (t MessageType) String() string {
	switch t {
	case MsgConnect:
		return "Connect"
	case MsgConnectAck:
		return "ConnectAck"
	case MsgData:
		return "Data"
	case MsgClose:
		return "Close"
	case MsgError:
		return "Error"
	case MsgConnectDirect:
		return "ConnectDirect"
	case MsgCredentialUpdate:
		return "CredentialUpdate"
	case MsgPing:
		return "Ping"
	case MsgPong:
		return "Pong"
	case MsgShutdown:
		return "Shutdown"
	case MsgBackendConfig:
		return "BackendConfig"
	case MsgLog:
		return "Log"
	default:
		return fmt.Sprintf("Unknown(%d)", t)
	}
}
