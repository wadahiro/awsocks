package ssm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// HTTPClient implements SSM API calls using direct HTTP requests
type HTTPClient struct {
	httpClient  *http.Client
	credentials aws.CredentialsProvider
	region      string
	signer      *v4.Signer
}

// NewHTTPClient creates a new HTTP-based SSM client
func NewHTTPClient(cfg aws.Config) *HTTPClient {
	return &HTTPClient{
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		credentials: cfg.Credentials,
		region:      cfg.Region,
		signer:      v4.NewSigner(),
	}
}

// StartSession calls the SSM StartSession API
func (c *HTTPClient) StartSession(ctx context.Context, input *StartSessionInput) (*StartSessionOutput, error) {
	reqBody := startSessionRequest{
		Target:       input.Target,
		DocumentName: input.DocumentName,
		Parameters:   input.Parameters,
	}

	respBody, err := c.doRequest(ctx, "StartSession", reqBody)
	if err != nil {
		return nil, err
	}

	var resp startSessionResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse StartSession response: %w", err)
	}

	return &StartSessionOutput{
		SessionId:  resp.SessionId,
		StreamUrl:  resp.StreamUrl,
		TokenValue: resp.TokenValue,
	}, nil
}

// TerminateSession calls the SSM TerminateSession API
func (c *HTTPClient) TerminateSession(ctx context.Context, input *TerminateSessionInput) (*TerminateSessionOutput, error) {
	reqBody := terminateSessionRequest{
		SessionId: input.SessionId,
	}

	respBody, err := c.doRequest(ctx, "TerminateSession", reqBody)
	if err != nil {
		return nil, err
	}

	var resp terminateSessionResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse TerminateSession response: %w", err)
	}

	return &TerminateSessionOutput{
		SessionId: resp.SessionId,
	}, nil
}

func (c *HTTPClient) doRequest(ctx context.Context, action string, reqBody interface{}) ([]byte, error) {
	endpoint := fmt.Sprintf("https://ssm.%s.amazonaws.com/", c.region)

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers for AWS JSON 1.1 protocol
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AmazonSSM."+action)

	// Get credentials and sign the request
	creds, err := c.credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve credentials: %w", err)
	}

	// Calculate payload hash
	payloadHash := sha256.Sum256(body)
	payloadHashHex := hex.EncodeToString(payloadHash[:])

	err = c.signer.SignHTTP(ctx, creds, req, payloadHashHex, "ssm", c.region, time.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Parse error response
		var errorResp ssmErrorResponse
		if jsonErr := json.Unmarshal(respBody, &errorResp); jsonErr == nil && errorResp.Type != "" {
			return nil, fmt.Errorf("SSM API error: %s - %s", errorResp.Type, errorResp.Message)
		}
		return nil, fmt.Errorf("SSM API error: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// Request/Response structures for JSON marshaling

type startSessionRequest struct {
	Target       string              `json:"Target"`
	DocumentName string              `json:"DocumentName,omitempty"`
	Parameters   map[string][]string `json:"Parameters,omitempty"`
}

type startSessionResponse struct {
	SessionId  string `json:"SessionId"`
	StreamUrl  string `json:"StreamUrl"`
	TokenValue string `json:"TokenValue"`
}

type terminateSessionRequest struct {
	SessionId string `json:"SessionId"`
}

type terminateSessionResponse struct {
	SessionId string `json:"SessionId"`
}

type ssmErrorResponse struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
}
