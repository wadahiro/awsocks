// Package fakessm provides a fake SSM server for testing
package fakessm

import (
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/wadahiro/awsocks/internal/backend/ssm/datachannel"
)

// OpenDataChannelInput represents the initial JSON message from client
type OpenDataChannelInput struct {
	MessageSchemaVersion string `json:"MessageSchemaVersion"`
	RequestID            string `json:"RequestId"`
	TokenValue           string `json:"TokenValue"`
	ClientID             string `json:"ClientId,omitempty"`
}

// NewHandshakeRequestMessage creates a handshake request message from SSM agent
func NewHandshakeRequestMessage(agentVersion string, sessionType string, seqNum int64) (*datachannel.ClientMessage, error) {
	req := &datachannel.HandshakeRequest{
		AgentVersion: agentVersion,
		RequestedClientActions: []datachannel.RequestedClientAction{
			{
				ActionType:       datachannel.ActionTypeSessionType,
				ActionParameters: json.RawMessage(`{"SessionType":"` + sessionType + `"}`),
			},
		},
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	payloadHash := sha256.Sum256(payload)
	return &datachannel.ClientMessage{
		HeaderLength:   datachannel.ClientMessageHeaderLength,
		MessageType:    datachannel.OutputStreamMessage,
		SchemaVersion:  1,
		CreatedDate:    uint64(time.Now().UnixMilli()),
		SequenceNumber: seqNum,
		Flags:          0,
		MessageID:      uuid.New(),
		PayloadDigest:  payloadHash[:],
		PayloadType:    datachannel.PayloadTypeHandshakeRequest,
		PayloadLength:  uint32(len(payload)),
		Payload:        payload,
	}, nil
}

// NewHandshakeCompleteMessage creates a handshake complete message from SSM agent
func NewHandshakeCompleteMessage(seqNum int64) (*datachannel.ClientMessage, error) {
	complete := &datachannel.HandshakeComplete{
		HandshakeTimeToComplete: time.Now().UnixMilli(),
		CustomerMessage:         "",
	}

	payload, err := json.Marshal(complete)
	if err != nil {
		return nil, err
	}

	payloadHash := sha256.Sum256(payload)
	return &datachannel.ClientMessage{
		HeaderLength:   datachannel.ClientMessageHeaderLength,
		MessageType:    datachannel.OutputStreamMessage,
		SchemaVersion:  1,
		CreatedDate:    uint64(time.Now().UnixMilli()),
		SequenceNumber: seqNum,
		Flags:          0,
		MessageID:      uuid.New(),
		PayloadDigest:  payloadHash[:],
		PayloadType:    datachannel.PayloadTypeHandshakeComplete,
		PayloadLength:  uint32(len(payload)),
		Payload:        payload,
	}, nil
}

// NewOutputDataMessage creates an output data message from SSM agent
func NewOutputDataMessage(payload []byte, seqNum int64) *datachannel.ClientMessage {
	payloadHash := sha256.Sum256(payload)
	return &datachannel.ClientMessage{
		HeaderLength:   datachannel.ClientMessageHeaderLength,
		MessageType:    datachannel.OutputStreamMessage,
		SchemaVersion:  1,
		CreatedDate:    uint64(time.Now().UnixMilli()),
		SequenceNumber: seqNum,
		Flags:          0,
		MessageID:      uuid.New(),
		PayloadDigest:  payloadHash[:],
		PayloadType:    datachannel.PayloadTypeOutput,
		PayloadLength:  uint32(len(payload)),
		Payload:        payload,
	}
}

// NewAcknowledgeMessage creates an ACK message from SSM agent
func NewAcknowledgeMessage(messageType string, messageID uuid.UUID, seqNum int64) (*datachannel.ClientMessage, error) {
	ack := &datachannel.AcknowledgeContent{
		AcknowledgedMessageType:           messageType,
		AcknowledgedMessageID:             messageID.String(),
		AcknowledgedMessageSequenceNumber: seqNum,
		IsSequentialMessage:               true,
	}

	payload, err := json.Marshal(ack)
	if err != nil {
		return nil, err
	}

	payloadHash := sha256.Sum256(payload)
	return &datachannel.ClientMessage{
		HeaderLength:   datachannel.ClientMessageHeaderLength,
		MessageType:    datachannel.AcknowledgeMessage,
		SchemaVersion:  1,
		CreatedDate:    uint64(time.Now().UnixMilli()),
		SequenceNumber: 0,
		Flags:          0,
		MessageID:      uuid.New(),
		PayloadDigest:  payloadHash[:],
		PayloadType:    datachannel.PayloadTypeOutput,
		PayloadLength:  uint32(len(payload)),
		Payload:        payload,
	}, nil
}
