package protocol

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessage_Encode_Decode(t *testing.T) {
	tests := []struct {
		name    string
		message *Message
	}{
		{
			name: "connect message",
			message: &Message{
				Type:    MsgConnect,
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
		{
			name: "backend config",
			message: &Message{
				Type:    MsgBackendConfig,
				ConnID:  0,
				Payload: []byte(`{"type":"ssm","instanceId":"i-12345"}`),
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

func TestNewCredentialUpdateMessage(t *testing.T) {
	expiration := time.Unix(1234567890, 0)
	creds := CredentialPayload{
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		SessionToken:    "token123",
		Expiration:      expiration,
	}

	msg := NewCredentialUpdateMessage(creds)

	assert.Equal(t, MsgCredentialUpdate, msg.Type)
	assert.Contains(t, string(msg.Payload), "AKIAIOSFODNN7EXAMPLE")
	assert.Contains(t, string(msg.Payload), "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	assert.Contains(t, string(msg.Payload), "token123")
	assert.Contains(t, string(msg.Payload), "1234567890")
}

func TestMessageType_String(t *testing.T) {
	tests := []struct {
		msgType  MessageType
		expected string
	}{
		{MsgConnect, "Connect"},
		{MsgConnectAck, "ConnectAck"},
		{MsgData, "Data"},
		{MsgClose, "Close"},
		{MsgError, "Error"},
		{MsgCredentialUpdate, "CredentialUpdate"},
		{MsgPing, "Ping"},
		{MsgPong, "Pong"},
		{MsgShutdown, "Shutdown"},
		{MsgBackendConfig, "BackendConfig"},
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
	msg := NewConnectMessage(1, "tcp", "example.com:443")

	err := WriteMessage(&buf, msg)
	require.NoError(t, err)

	decoded, err := ReadMessage(&buf)
	require.NoError(t, err)

	assert.Equal(t, MsgConnect, decoded.Type)
	assert.Equal(t, uint32(1), decoded.ConnID)

	network, address, err := ParseConnectPayload(decoded.Payload)
	require.NoError(t, err)
	assert.Equal(t, "tcp", network)
	assert.Equal(t, "example.com:443", address)
}

func TestBackendConfigMessage_RoundTrip(t *testing.T) {
	original := &BackendConfigPayload{
		Type:             "sshproxy",
		InstanceID:       "i-0123456789abcdef0",
		Region:           "ap-northeast-1",
		SSHUser:          "ec2-user",
		SSHKeyContent:    []byte("-----BEGIN OPENSSH PRIVATE KEY-----\ntest\n-----END OPENSSH PRIVATE KEY-----"),
		SSHKeyPassphrase: "secret",
	}

	msg := NewBackendConfigMessage(original)

	// Encode and decode
	var buf bytes.Buffer
	err := WriteMessage(&buf, msg)
	require.NoError(t, err)

	decoded, err := ReadMessage(&buf)
	require.NoError(t, err)
	assert.Equal(t, MsgBackendConfig, decoded.Type)

	parsed, err := ParseBackendConfigPayload(decoded.Payload)
	require.NoError(t, err)
	assert.Equal(t, original.Type, parsed.Type)
	assert.Equal(t, original.InstanceID, parsed.InstanceID)
	assert.Equal(t, original.Region, parsed.Region)
	assert.Equal(t, original.SSHUser, parsed.SSHUser)
	assert.Equal(t, original.SSHKeyContent, parsed.SSHKeyContent)
	assert.Equal(t, original.SSHKeyPassphrase, parsed.SSHKeyPassphrase)
}

func TestParseBackendConfigPayload_Invalid(t *testing.T) {
	_, err := ParseBackendConfigPayload([]byte("invalid json"))
	assert.Error(t, err)
}
