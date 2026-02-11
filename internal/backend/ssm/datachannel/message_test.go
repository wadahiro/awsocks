package datachannel

import (
	"bytes"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientMessage_Serialize(t *testing.T) {
	msgID := uuid.MustParse("12345678-1234-1234-1234-123456789abc")
	payload := []byte("test payload")
	payloadHash := sha256.Sum256(payload)

	msg := &ClientMessage{
		HeaderLength:   ClientMessageHeaderLength,
		MessageType:    InputStreamMessage,
		SchemaVersion:  1,
		CreatedDate:    uint64(time.Now().UnixMilli()),
		SequenceNumber: 1,
		Flags:          0,
		MessageID:      msgID,
		PayloadDigest:  payloadHash[:],
		PayloadType:    PayloadTypeOutput,
		PayloadLength:  uint32(len(payload)),
		Payload:        payload,
	}

	data, err := msg.Serialize()
	require.NoError(t, err)

	// Header length (116) + payload length field (4) + payload
	expectedLen := int(ClientMessageHeaderLength) + 4 + len(payload)
	assert.Equal(t, expectedLen, len(data))
}

func TestClientMessage_Deserialize(t *testing.T) {
	msgID := uuid.MustParse("12345678-1234-1234-1234-123456789abc")
	payload := []byte("test payload")
	payloadHash := sha256.Sum256(payload)
	createdDate := uint64(time.Now().UnixMilli())

	original := &ClientMessage{
		HeaderLength:   ClientMessageHeaderLength,
		MessageType:    OutputStreamMessage,
		SchemaVersion:  1,
		CreatedDate:    createdDate,
		SequenceNumber: 42,
		Flags:          0,
		MessageID:      msgID,
		PayloadDigest:  payloadHash[:],
		PayloadType:    PayloadTypeOutput,
		PayloadLength:  uint32(len(payload)),
		Payload:        payload,
	}

	data, err := original.Serialize()
	require.NoError(t, err)

	deserialized := &ClientMessage{}
	err = deserialized.Deserialize(data)
	require.NoError(t, err)

	assert.Equal(t, original.HeaderLength, deserialized.HeaderLength)
	assert.Equal(t, original.MessageType, deserialized.MessageType)
	assert.Equal(t, original.SchemaVersion, deserialized.SchemaVersion)
	assert.Equal(t, original.CreatedDate, deserialized.CreatedDate)
	assert.Equal(t, original.SequenceNumber, deserialized.SequenceNumber)
	assert.Equal(t, original.Flags, deserialized.Flags)
	assert.Equal(t, original.MessageID, deserialized.MessageID)
	assert.Equal(t, original.PayloadType, deserialized.PayloadType)
	assert.Equal(t, original.PayloadLength, deserialized.PayloadLength)
	assert.True(t, bytes.Equal(original.Payload, deserialized.Payload))
}

func TestClientMessage_RoundTrip(t *testing.T) {
	testCases := []struct {
		name        string
		messageType string
		payloadType PayloadType
		payload     []byte
		seqNum      int64
	}{
		{
			name:        "input stream message",
			messageType: InputStreamMessage,
			payloadType: PayloadTypeOutput,
			payload:     []byte("hello world"),
			seqNum:      1,
		},
		{
			name:        "output stream message",
			messageType: OutputStreamMessage,
			payloadType: PayloadTypeOutput,
			payload:     []byte("response data"),
			seqNum:      2,
		},
		{
			name:        "acknowledge message",
			messageType: AcknowledgeMessage,
			payloadType: PayloadTypeOutput,
			payload:     []byte(`{"AcknowledgedMessageType":"output_stream_data","AcknowledgedMessageId":"12345678-1234-1234-1234-123456789abc","AcknowledgedMessageSequenceNumber":1,"IsSequentialMessage":true}`),
			seqNum:      0,
		},
		{
			name:        "handshake request",
			messageType: InputStreamMessage,
			payloadType: PayloadTypeHandshakeRequest,
			payload:     []byte(`{"AgentVersion":"3.0.0","RequestedClientActions":[]}`),
			seqNum:      0,
		},
		{
			name:        "empty payload",
			messageType: InputStreamMessage,
			payloadType: PayloadTypeOutput,
			payload:     []byte{},
			seqNum:      0,
		},
		{
			name:        "large payload",
			messageType: OutputStreamMessage,
			payloadType: PayloadTypeOutput,
			payload:     bytes.Repeat([]byte("x"), 1024*64), // 64KB
			seqNum:      100,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			msgID := uuid.New()
			payloadHash := sha256.Sum256(tc.payload)

			original := &ClientMessage{
				HeaderLength:   ClientMessageHeaderLength,
				MessageType:    tc.messageType,
				SchemaVersion:  1,
				CreatedDate:    uint64(time.Now().UnixMilli()),
				SequenceNumber: tc.seqNum,
				Flags:          0,
				MessageID:      msgID,
				PayloadDigest:  payloadHash[:],
				PayloadType:    tc.payloadType,
				PayloadLength:  uint32(len(tc.payload)),
				Payload:        tc.payload,
			}

			data, err := original.Serialize()
			require.NoError(t, err)

			deserialized := &ClientMessage{}
			err = deserialized.Deserialize(data)
			require.NoError(t, err)

			assert.Equal(t, original.MessageType, deserialized.MessageType)
			assert.Equal(t, original.SequenceNumber, deserialized.SequenceNumber)
			assert.Equal(t, original.PayloadType, deserialized.PayloadType)
			assert.True(t, bytes.Equal(original.Payload, deserialized.Payload))
		})
	}
}

func TestAcknowledgeContent_Marshal(t *testing.T) {
	msgID := uuid.MustParse("12345678-1234-1234-1234-123456789abc")

	ack := &AcknowledgeContent{
		AcknowledgedMessageType:           OutputStreamMessage,
		AcknowledgedMessageID:             msgID.String(),
		AcknowledgedMessageSequenceNumber: 42,
		IsSequentialMessage:               true,
	}

	data, err := ack.Marshal()
	require.NoError(t, err)
	assert.Contains(t, string(data), `"AcknowledgedMessageType":"output_stream_data"`)
	assert.Contains(t, string(data), `"AcknowledgedMessageId":"12345678-1234-1234-1234-123456789abc"`)
	assert.Contains(t, string(data), `"AcknowledgedMessageSequenceNumber":42`)
	assert.Contains(t, string(data), `"IsSequentialMessage":true`)
}

func TestAcknowledgeContent_Unmarshal(t *testing.T) {
	data := []byte(`{"AcknowledgedMessageType":"output_stream_data","AcknowledgedMessageId":"12345678-1234-1234-1234-123456789abc","AcknowledgedMessageSequenceNumber":42,"IsSequentialMessage":true}`)

	ack := &AcknowledgeContent{}
	err := ack.Unmarshal(data)
	require.NoError(t, err)

	assert.Equal(t, OutputStreamMessage, ack.AcknowledgedMessageType)
	assert.Equal(t, "12345678-1234-1234-1234-123456789abc", ack.AcknowledgedMessageID)
	assert.Equal(t, int64(42), ack.AcknowledgedMessageSequenceNumber)
	assert.True(t, ack.IsSequentialMessage)
}

func TestClientMessage_MessageType_Padding(t *testing.T) {
	// MessageType is 32 bytes, padded with spaces
	msgID := uuid.New()
	payload := []byte("test")
	payloadHash := sha256.Sum256(payload)

	msg := &ClientMessage{
		HeaderLength:   ClientMessageHeaderLength,
		MessageType:    InputStreamMessage, // "input_stream_data" is 17 chars
		SchemaVersion:  1,
		CreatedDate:    uint64(time.Now().UnixMilli()),
		SequenceNumber: 1,
		Flags:          0,
		MessageID:      msgID,
		PayloadDigest:  payloadHash[:],
		PayloadType:    PayloadTypeOutput,
		PayloadLength:  uint32(len(payload)),
		Payload:        payload,
	}

	data, err := msg.Serialize()
	require.NoError(t, err)

	// Check that message type field is 32 bytes starting at offset 4
	messageTypeBytes := data[ClientMessageTypeOffset : ClientMessageTypeOffset+ClientMessageTypeLength]
	assert.Equal(t, 32, len(messageTypeBytes))

	// Deserialize and verify message type is correctly parsed
	deserialized := &ClientMessage{}
	err = deserialized.Deserialize(data)
	require.NoError(t, err)
	assert.Equal(t, InputStreamMessage, deserialized.MessageType)
}

func TestClientMessage_UUID_Serialization(t *testing.T) {
	msgID := uuid.MustParse("12345678-1234-1234-1234-123456789abc")
	payload := []byte("test")
	payloadHash := sha256.Sum256(payload)

	msg := &ClientMessage{
		HeaderLength:   ClientMessageHeaderLength,
		MessageType:    InputStreamMessage,
		SchemaVersion:  1,
		CreatedDate:    uint64(time.Now().UnixMilli()),
		SequenceNumber: 1,
		Flags:          0,
		MessageID:      msgID,
		PayloadDigest:  payloadHash[:],
		PayloadType:    PayloadTypeOutput,
		PayloadLength:  uint32(len(payload)),
		Payload:        payload,
	}

	data, err := msg.Serialize()
	require.NoError(t, err)

	deserialized := &ClientMessage{}
	err = deserialized.Deserialize(data)
	require.NoError(t, err)

	assert.Equal(t, msgID, deserialized.MessageID)
}

func TestClientMessage_Deserialize_InvalidInput(t *testing.T) {
	testCases := []struct {
		name  string
		input []byte
	}{
		{
			name:  "nil input",
			input: nil,
		},
		{
			name:  "empty input",
			input: []byte{},
		},
		{
			name:  "too short for header",
			input: make([]byte, 50),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			msg := &ClientMessage{}
			err := msg.Deserialize(tc.input)
			assert.Error(t, err)
		})
	}
}

func TestNewInputStreamMessage(t *testing.T) {
	payload := []byte("test data")
	seqNum := int64(5)

	msg := NewInputStreamMessage(payload, seqNum)

	assert.Equal(t, ClientMessageHeaderLength, msg.HeaderLength)
	assert.Equal(t, InputStreamMessage, msg.MessageType)
	assert.Equal(t, uint32(1), msg.SchemaVersion)
	assert.Equal(t, seqNum, msg.SequenceNumber)
	assert.Equal(t, PayloadTypeOutput, msg.PayloadType)
	assert.Equal(t, uint32(len(payload)), msg.PayloadLength)
	assert.True(t, bytes.Equal(payload, msg.Payload))

	// Verify payload digest
	expectedHash := sha256.Sum256(payload)
	assert.True(t, bytes.Equal(expectedHash[:], msg.PayloadDigest))
}

func TestNewAcknowledgeMessage(t *testing.T) {
	originalMsgID := uuid.New()
	originalSeqNum := int64(10)

	msg := NewAcknowledgeMessage(OutputStreamMessage, originalMsgID, originalSeqNum)

	assert.Equal(t, ClientMessageHeaderLength, msg.HeaderLength)
	assert.Equal(t, AcknowledgeMessage, msg.MessageType)
	assert.Equal(t, uint32(1), msg.SchemaVersion)
	assert.Equal(t, int64(0), msg.SequenceNumber) // ACK messages have sequence 0
	assert.Equal(t, PayloadTypeOutput, msg.PayloadType)

	// Verify the payload contains acknowledge content
	ack := &AcknowledgeContent{}
	err := ack.Unmarshal(msg.Payload)
	require.NoError(t, err)
	assert.Equal(t, OutputStreamMessage, ack.AcknowledgedMessageType)
	assert.Equal(t, originalMsgID.String(), ack.AcknowledgedMessageID)
	assert.Equal(t, originalSeqNum, ack.AcknowledgedMessageSequenceNumber)
	assert.True(t, ack.IsSequentialMessage)
}
