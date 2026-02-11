package datachannel

import (
	"encoding/json"
	"fmt"
)

// ActionType constants
type ActionType string

const (
	ActionTypeKMSEncryption ActionType = "KMSEncryption"
	ActionTypeSessionType   ActionType = "SessionType"
)

// ActionStatus constants
type ActionStatus int

const (
	ActionStatusSuccess     ActionStatus = 1
	ActionStatusFailed      ActionStatus = 2
	ActionStatusUnsupported ActionStatus = 3
)

// HandshakeRequest represents the handshake request from SSM agent
type HandshakeRequest struct {
	AgentVersion           string                  `json:"AgentVersion"`
	RequestedClientActions []RequestedClientAction `json:"RequestedClientActions"`
}

// RequestedClientAction represents an action requested by the agent
type RequestedClientAction struct {
	ActionType       ActionType      `json:"ActionType"`
	ActionParameters json.RawMessage `json:"ActionParameters"`
}

// Parse parses the handshake request payload
func (h *HandshakeRequest) Parse(data []byte) error {
	return json.Unmarshal(data, h)
}

// HandshakeResponse represents the response to handshake request
type HandshakeResponse struct {
	ClientVersion          string                  `json:"ClientVersion"`
	ProcessedClientActions []ProcessedClientAction `json:"ProcessedClientActions"`
	Errors                 []string                `json:"Errors,omitempty"`
}

// ProcessedClientAction represents a processed action result
type ProcessedClientAction struct {
	ActionType   ActionType   `json:"ActionType"`
	ActionStatus ActionStatus `json:"ActionStatus"`
	ActionResult interface{}  `json:"ActionResult,omitempty"`
	Error        string       `json:"Error,omitempty"`
}

// NewHandshakeResponse creates a new handshake response
func NewHandshakeResponse(clientVersion string) *HandshakeResponse {
	return &HandshakeResponse{
		ClientVersion:          clientVersion,
		ProcessedClientActions: []ProcessedClientAction{},
	}
}

// AddProcessedAction adds a processed action to the response
func (h *HandshakeResponse) AddProcessedAction(actionType ActionType, status ActionStatus, result interface{}, errMsg string) {
	h.ProcessedClientActions = append(h.ProcessedClientActions, ProcessedClientAction{
		ActionType:   actionType,
		ActionStatus: status,
		ActionResult: result,
		Error:        errMsg,
	})
}

// Build serializes the response to JSON
func (h *HandshakeResponse) Build() ([]byte, error) {
	return json.Marshal(h)
}

// HandshakeComplete represents the handshake complete message
type HandshakeComplete struct {
	HandshakeTimeToComplete int64  `json:"HandshakeTimeToComplete"`
	CustomerMessage         string `json:"CustomerMessage"`
}

// Parse parses the handshake complete payload
func (h *HandshakeComplete) Parse(data []byte) error {
	return json.Unmarshal(data, h)
}

// SessionTypeParameters represents session type action parameters
type SessionTypeParameters struct {
	SessionType string            `json:"SessionType"`
	Properties  map[string]string `json:"Properties"`
}

// Parse parses session type parameters
func (s *SessionTypeParameters) Parse(data []byte) error {
	return json.Unmarshal(data, s)
}

// KMSEncryptionRequest represents KMS encryption request parameters
type KMSEncryptionRequest struct {
	KMSKeyID string `json:"KMSKeyId"`
}

// Parse parses KMS encryption request
func (k *KMSEncryptionRequest) Parse(data []byte) error {
	return json.Unmarshal(data, k)
}

// HandshakeProcessor handles the handshake process
type HandshakeProcessor struct {
	clientVersion string
	sessionType   string
	properties    map[string]string
}

// NewHandshakeProcessor creates a new handshake processor
func NewHandshakeProcessor(clientVersion string) *HandshakeProcessor {
	return &HandshakeProcessor{
		clientVersion: clientVersion,
		properties:    make(map[string]string),
	}
}

// ProcessHandshake processes a handshake request and returns a response
func (p *HandshakeProcessor) ProcessHandshake(data []byte) (*HandshakeResponse, error) {
	req := &HandshakeRequest{}
	if err := req.Parse(data); err != nil {
		return nil, fmt.Errorf("failed to parse handshake request: %w", err)
	}

	resp := NewHandshakeResponse(p.clientVersion)

	for _, action := range req.RequestedClientActions {
		switch action.ActionType {
		case ActionTypeSessionType:
			if err := p.processSessionType(action.ActionParameters, resp); err != nil {
				return nil, err
			}
		case ActionTypeKMSEncryption:
			// KMS encryption is not supported - mark as unsupported
			resp.AddProcessedAction(ActionTypeKMSEncryption, ActionStatusUnsupported, nil, "KMS encryption not supported")
		default:
			// Unknown action type - mark as unsupported
			resp.AddProcessedAction(action.ActionType, ActionStatusUnsupported, nil, "unknown action type")
		}
	}

	return resp, nil
}

// processSessionType handles the session type action
func (p *HandshakeProcessor) processSessionType(params json.RawMessage, resp *HandshakeResponse) error {
	stp := &SessionTypeParameters{}
	if err := stp.Parse(params); err != nil {
		resp.AddProcessedAction(ActionTypeSessionType, ActionStatusFailed, nil, err.Error())
		return nil
	}

	p.sessionType = stp.SessionType
	p.properties = stp.Properties

	result := map[string]string{
		"SessionType": stp.SessionType,
	}
	resp.AddProcessedAction(ActionTypeSessionType, ActionStatusSuccess, result, "")

	return nil
}

// SessionType returns the negotiated session type
func (p *HandshakeProcessor) SessionType() string {
	return p.sessionType
}

// Properties returns the session properties
func (p *HandshakeProcessor) Properties() map[string]string {
	return p.properties
}
