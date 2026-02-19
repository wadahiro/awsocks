package protocol

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessage_Encode_Decode(t *testing.T) {
	tests := []struct {
		name    string
		message *Message
	}{
		{
			name: "connect direct message",
			message: &Message{
				Type:    MsgConnectDirect,
				ConnID:  42,
				Payload: []byte("tcp:example.com:80"),
			},
		},
		{
			name: "data message",
			message: &Message{
				Type:    MsgData,
				ConnID:  123,
				Payload: []byte("hello world"),
			},
		},
		{
			name: "empty payload",
			message: &Message{
				Type:   MsgClose,
				ConnID: 1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := tt.message.Encode()
			require.NoError(t, err)

			decoded, err := ReadMessage(bytes.NewReader(encoded))
			require.NoError(t, err)

			assert.Equal(t, tt.message.Type, decoded.Type)
			assert.Equal(t, tt.message.ConnID, decoded.ConnID)
			assert.Equal(t, tt.message.Payload, decoded.Payload)
		})
	}
}

func TestParseConnectPayload(t *testing.T) {
	tests := []struct {
		name     string
		payload  string
		network  string
		address  string
		hasError bool
	}{
		{
			name:    "tcp connection",
			payload: "tcp:example.com:80",
			network: "tcp",
			address: "example.com:80",
		},
		{
			name:    "udp connection",
			payload: "udp:8.8.8.8:53",
			network: "udp",
			address: "8.8.8.8:53",
		},
		{
			name:    "tcp with ipv6",
			payload: "tcp:[::1]:8080",
			network: "tcp",
			address: "[::1]:8080",
		},
		{
			name:     "invalid format",
			payload:  "invalid",
			hasError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			network, address, err := ParseConnectPayload([]byte(tt.payload))
			if tt.hasError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.network, network)
				assert.Equal(t, tt.address, address)
			}
		})
	}
}

func TestMessageType_String(t *testing.T) {
	tests := []struct {
		msgType  MessageType
		expected string
	}{
		{MsgConnectAck, "ConnectAck"},
		{MsgData, "Data"},
		{MsgClose, "Close"},
		{MsgError, "Error"},
		{MsgConnectDirect, "ConnectDirect"},
		{MsgPing, "Ping"},
		{MsgPong, "Pong"},
		{MsgShutdown, "Shutdown"},
		{MsgLog, "Log"},
		{MessageType(0xFF), "Unknown(255)"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.msgType.String())
		})
	}
}

func TestWriteMessage(t *testing.T) {
	var buf bytes.Buffer
	msg := NewConnectDirectMessage(1, "tcp", "example.com:443")

	err := WriteMessage(&buf, msg)
	require.NoError(t, err)

	decoded, err := ReadMessage(&buf)
	require.NoError(t, err)

	assert.Equal(t, MsgConnectDirect, decoded.Type)
	assert.Equal(t, uint32(1), decoded.ConnID)

	network, address, err := ParseConnectPayload(decoded.Payload)
	require.NoError(t, err)
	assert.Equal(t, "tcp", network)
	assert.Equal(t, "example.com:443", address)
}
