package datachannel

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Message type constants
const (
	InputStreamMessage      = "input_stream_data"
	OutputStreamMessage     = "output_stream_data"
	AcknowledgeMessage      = "acknowledge"
	ChannelClosedMessage    = "channel_closed"
	StartPublicationMessage = "start_publication"
	PausePublicationMessage = "pause_publication"
)

// PayloadType constants
type PayloadType uint32

const (
	PayloadTypeOutput            PayloadType = 1
	PayloadTypeError             PayloadType = 2
	PayloadTypeSize              PayloadType = 3
	PayloadTypeParameter         PayloadType = 4
	PayloadTypeHandshakeRequest  PayloadType = 5
	PayloadTypeHandshakeResponse PayloadType = 6
	PayloadTypeHandshakeComplete PayloadType = 7
	PayloadTypeEncChallengeReq   PayloadType = 8
	PayloadTypeEncChallengeResp  PayloadType = 9
	PayloadTypeFlag              PayloadType = 10
	PayloadTypeStdErr            PayloadType = 11
	PayloadTypeExitCode          PayloadType = 12
)

// FlagType constants for Flag payload
type FlagType uint32

const (
	FlagDisconnectToPort   FlagType = 1
	FlagTerminateSession   FlagType = 2
	FlagConnectToPortError FlagType = 3
)

// Field lengths
const (
	ClientMessageHLLength             = 4
	ClientMessageTypeLength           = 32
	ClientMessageSchemaVersionLength  = 4
	ClientMessageCreatedDateLength    = 8
	ClientMessageSequenceNumberLength = 8
	ClientMessageFlagsLength          = 8
	ClientMessageIDLength             = 16
	ClientMessagePayloadDigestLength  = 32
	ClientMessagePayloadTypeLength    = 4
	ClientMessagePayloadLengthLength  = 4
)

// Field offsets
const (
	ClientMessageHLOffset             = 0
	ClientMessageTypeOffset           = 4
	ClientMessageSchemaVersionOffset  = 36
	ClientMessageCreatedDateOffset    = 40
	ClientMessageSequenceNumberOffset = 48
	ClientMessageFlagsOffset          = 56
	ClientMessageIDOffset             = 64
	ClientMessagePayloadDigestOffset  = 80
	ClientMessagePayloadTypeOffset    = 112
	ClientMessagePayloadLengthOffset  = 116
	ClientMessagePayloadOffset        = 120
)

// ClientMessageHeaderLength is the fixed header size (without payload)
const ClientMessageHeaderLength uint32 = 116

// ClientMessage represents a message exchanged with SSM agent
type ClientMessage struct {
	HeaderLength   uint32
	MessageType    string
	SchemaVersion  uint32
	CreatedDate    uint64
	SequenceNumber int64
	Flags          uint64
	MessageID      uuid.UUID
	PayloadDigest  []byte
	PayloadType    PayloadType
	PayloadLength  uint32
	Payload        []byte
}

// Serialize converts the message to bytes
func (m *ClientMessage) Serialize() ([]byte, error) {
	// Total size: header length field + header + payload
	totalLen := ClientMessageHLLength + int(m.HeaderLength) + int(m.PayloadLength)
	buf := make([]byte, totalLen)

	// Header length (4 bytes at offset 0)
	binary.BigEndian.PutUint32(buf[ClientMessageHLOffset:], m.HeaderLength)

	// Message type (32 bytes at offset 4, space-padded)
	messageTypeBytes := make([]byte, ClientMessageTypeLength)
	for i := range messageTypeBytes {
		messageTypeBytes[i] = ' '
	}
	copy(messageTypeBytes, m.MessageType)
	copy(buf[ClientMessageTypeOffset:], messageTypeBytes)

	// Schema version (4 bytes at offset 36)
	binary.BigEndian.PutUint32(buf[ClientMessageSchemaVersionOffset:], m.SchemaVersion)

	// Created date (8 bytes at offset 40)
	binary.BigEndian.PutUint64(buf[ClientMessageCreatedDateOffset:], m.CreatedDate)

	// Sequence number (8 bytes at offset 48)
	binary.BigEndian.PutUint64(buf[ClientMessageSequenceNumberOffset:], uint64(m.SequenceNumber))

	// Flags (8 bytes at offset 56)
	binary.BigEndian.PutUint64(buf[ClientMessageFlagsOffset:], m.Flags)

	// Message ID (16 bytes at offset 64) - UUID with bytes swapped (AWS SSM protocol format)
	// AWS SSM expects the last 8 bytes first, then the first 8 bytes
	uuidBytes, err := m.MessageID.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal UUID: %w", err)
	}
	// Swap: last 8 bytes go first, then first 8 bytes
	swappedUUID := append(uuidBytes[8:], uuidBytes[:8]...)
	copy(buf[ClientMessageIDOffset:], swappedUUID)

	// Payload digest (32 bytes at offset 80)
	copy(buf[ClientMessagePayloadDigestOffset:], m.PayloadDigest)

	// Payload type (4 bytes at offset 112)
	binary.BigEndian.PutUint32(buf[ClientMessagePayloadTypeOffset:], uint32(m.PayloadType))

	// Payload length (4 bytes at offset 116)
	binary.BigEndian.PutUint32(buf[ClientMessagePayloadLengthOffset:], m.PayloadLength)

	// Payload (variable, starts at offset 120)
	copy(buf[ClientMessagePayloadOffset:], m.Payload)

	return buf, nil
}

// Deserialize parses bytes into a message
func (m *ClientMessage) Deserialize(data []byte) error {
	if len(data) < ClientMessagePayloadOffset {
		return fmt.Errorf("input too short: expected at least %d bytes, got %d", ClientMessagePayloadOffset, len(data))
	}

	// Header length
	m.HeaderLength = binary.BigEndian.Uint32(data[ClientMessageHLOffset:])

	// Message type (trim trailing spaces)
	messageTypeBytes := data[ClientMessageTypeOffset : ClientMessageTypeOffset+ClientMessageTypeLength]
	m.MessageType = strings.TrimRight(string(messageTypeBytes), " ")

	// Schema version
	m.SchemaVersion = binary.BigEndian.Uint32(data[ClientMessageSchemaVersionOffset:])

	// Created date
	m.CreatedDate = binary.BigEndian.Uint64(data[ClientMessageCreatedDateOffset:])

	// Sequence number
	m.SequenceNumber = int64(binary.BigEndian.Uint64(data[ClientMessageSequenceNumberOffset:]))

	// Flags
	m.Flags = binary.BigEndian.Uint64(data[ClientMessageFlagsOffset:])

	// Message ID (bytes are swapped in AWS SSM protocol format)
	// Unswap: last 8 bytes are actually first, and first 8 are actually last
	uuidData := data[ClientMessageIDOffset : ClientMessageIDOffset+ClientMessageIDLength]
	unswappedUUID := append(uuidData[8:], uuidData[:8]...)
	err := m.MessageID.UnmarshalBinary(unswappedUUID)
	if err != nil {
		return fmt.Errorf("failed to unmarshal UUID: %w", err)
	}

	// Payload digest
	m.PayloadDigest = make([]byte, ClientMessagePayloadDigestLength)
	copy(m.PayloadDigest, data[ClientMessagePayloadDigestOffset:ClientMessagePayloadDigestOffset+ClientMessagePayloadDigestLength])

	// Payload type
	m.PayloadType = PayloadType(binary.BigEndian.Uint32(data[ClientMessagePayloadTypeOffset:]))

	// Payload length
	m.PayloadLength = binary.BigEndian.Uint32(data[ClientMessagePayloadLengthOffset:])

	// Payload
	payloadStart := int(m.HeaderLength) + ClientMessageHLLength
	if len(data) < payloadStart+int(m.PayloadLength) {
		return fmt.Errorf("input too short for payload: expected %d bytes, got %d", payloadStart+int(m.PayloadLength), len(data))
	}
	m.Payload = make([]byte, m.PayloadLength)
	copy(m.Payload, data[payloadStart:payloadStart+int(m.PayloadLength)])

	return nil
}

// AcknowledgeContent is the payload of an acknowledge message
type AcknowledgeContent struct {
	AcknowledgedMessageType           string `json:"AcknowledgedMessageType"`
	AcknowledgedMessageID             string `json:"AcknowledgedMessageId"`
	AcknowledgedMessageSequenceNumber int64  `json:"AcknowledgedMessageSequenceNumber"`
	IsSequentialMessage               bool   `json:"IsSequentialMessage"`
}

// Marshal serializes the acknowledge content to JSON
func (a *AcknowledgeContent) Marshal() ([]byte, error) {
	return json.Marshal(a)
}

// Unmarshal deserializes JSON into acknowledge content
func (a *AcknowledgeContent) Unmarshal(data []byte) error {
	return json.Unmarshal(data, a)
}

// NewInputStreamMessage creates a new input stream message
func NewInputStreamMessage(payload []byte, sequenceNumber int64) *ClientMessage {
	payloadHash := sha256.Sum256(payload)
	return &ClientMessage{
		HeaderLength:   ClientMessageHeaderLength,
		MessageType:    InputStreamMessage,
		SchemaVersion:  1,
		CreatedDate:    uint64(time.Now().UnixMilli()),
		SequenceNumber: sequenceNumber,
		Flags:          0,
		MessageID:      uuid.New(),
		PayloadDigest:  payloadHash[:],
		PayloadType:    PayloadTypeOutput,
		PayloadLength:  uint32(len(payload)),
		Payload:        payload,
	}
}

// NewAcknowledgeMessage creates a new acknowledge message
func NewAcknowledgeMessage(messageType string, messageID uuid.UUID, sequenceNumber int64) *ClientMessage {
	ack := &AcknowledgeContent{
		AcknowledgedMessageType:           messageType,
		AcknowledgedMessageID:             messageID.String(),
		AcknowledgedMessageSequenceNumber: sequenceNumber,
		IsSequentialMessage:               true,
	}
	payload, _ := ack.Marshal()
	payloadHash := sha256.Sum256(payload)

	return &ClientMessage{
		HeaderLength:   ClientMessageHeaderLength,
		MessageType:    AcknowledgeMessage,
		SchemaVersion:  1,
		CreatedDate:    uint64(time.Now().UnixMilli()),
		SequenceNumber: 0,
		Flags:          0,
		MessageID:      uuid.New(),
		PayloadDigest:  payloadHash[:],
		PayloadType:    PayloadTypeOutput,
		PayloadLength:  uint32(len(payload)),
		Payload:        payload,
	}
}
