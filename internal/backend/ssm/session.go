package ssm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// SSMSession represents an active SSM session
type SSMSession struct {
	SessionID  string
	StreamURL  string
	TokenValue string
}

// StartSSMSession creates a new SSM session for SSH
func StartSSMSession(ctx context.Context, client SSMClient, instanceID, region string, creds aws.Credentials) (*SSMSession, error) {
	// Request SSH session directly (no port forwarding wrapper)
	docName := "AWS-StartSSHSession"
	params := map[string][]string{
		"portNumber": {"22"},
	}

	input := &StartSessionInput{
		Target:       instanceID,
		DocumentName: docName,
		Parameters:   params,
	}

	output, err := client.StartSession(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to start SSM session: %w", err)
	}

	if output.SessionId == "" || output.StreamUrl == "" || output.TokenValue == "" {
		return nil, fmt.Errorf("invalid session response: missing required fields")
	}

	return &SSMSession{
		SessionID:  output.SessionId,
		StreamURL:  output.StreamUrl,
		TokenValue: output.TokenValue,
	}, nil
}

// TerminateSSMSession terminates an SSM session
func TerminateSSMSession(ctx context.Context, client SSMClient, sessionID string) error {
	input := &TerminateSessionInput{
		SessionId: sessionID,
	}

	_, err := client.TerminateSession(ctx, input)
	return err
}

// OpenDataChannelInput is the initial message sent over WebSocket
type OpenDataChannelInput struct {
	MessageSchemaVersion string `json:"MessageSchemaVersion"`
	RequestId            string `json:"RequestId"`
	TokenValue           string `json:"TokenValue"`
	ClientId             string `json:"ClientId"`
}

// BuildOpenDataChannelInput creates the input for opening a data channel
func BuildOpenDataChannelInput(sessionID, tokenValue string) ([]byte, error) {
	input := OpenDataChannelInput{
		MessageSchemaVersion: "1.0",
		RequestId:            sessionID,
		TokenValue:           tokenValue,
		ClientId:             sessionID,
	}
	return json.Marshal(input)
}
