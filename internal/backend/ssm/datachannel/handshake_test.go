package datachannel

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandshakeRequest_Parse(t *testing.T) {
	testCases := []struct {
		name             string
		payload          string
		expectedVersion  string
		expectedActions  int
		expectedError    bool
	}{
		{
			name:            "basic request",
			payload:         `{"AgentVersion":"3.2.2016.0","RequestedClientActions":[]}`,
			expectedVersion: "3.2.2016.0",
			expectedActions: 0,
			expectedError:   false,
		},
		{
			name:            "request with session type action",
			payload:         `{"AgentVersion":"3.2.2016.0","RequestedClientActions":[{"ActionType":"SessionType","ActionParameters":{"SessionType":"Port","Properties":{}}}]}`,
			expectedVersion: "3.2.2016.0",
			expectedActions: 1,
			expectedError:   false,
		},
		{
			name:            "request with KMS encryption",
			payload:         `{"AgentVersion":"3.2.2016.0","RequestedClientActions":[{"ActionType":"KMSEncryption","ActionParameters":{"KMSKeyId":"arn:aws:kms:us-east-1:123456789012:key/12345678-1234-1234-1234-123456789012"}}]}`,
			expectedVersion: "3.2.2016.0",
			expectedActions: 1,
			expectedError:   false,
		},
		{
			name:          "invalid JSON",
			payload:       `{invalid}`,
			expectedError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := &HandshakeRequest{}
			err := req.Parse([]byte(tc.payload))

			if tc.expectedError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.expectedVersion, req.AgentVersion)
			assert.Len(t, req.RequestedClientActions, tc.expectedActions)
		})
	}
}

func TestHandshakeResponse_Build(t *testing.T) {
	resp := NewHandshakeResponse("1.0.0")

	// Add successful action
	resp.AddProcessedAction(ActionTypeSessionType, ActionStatusSuccess, map[string]string{
		"SessionType": "Port",
	}, "")

	data, err := resp.Build()
	require.NoError(t, err)

	assert.Contains(t, string(data), `"ClientVersion":"1.0.0"`)
	assert.Contains(t, string(data), `"ActionType":"SessionType"`)
	assert.Contains(t, string(data), `"ActionStatus":1`)
}

func TestHandshakeResponse_BuildWithError(t *testing.T) {
	resp := NewHandshakeResponse("1.0.0")

	// Add failed action
	resp.AddProcessedAction(ActionTypeKMSEncryption, ActionStatusFailed, nil, "KMS encryption not supported")

	data, err := resp.Build()
	require.NoError(t, err)

	assert.Contains(t, string(data), `"ActionStatus":2`)
	assert.Contains(t, string(data), `"Error":"KMS encryption not supported"`)
}

func TestHandshakeComplete_Parse(t *testing.T) {
	payload := `{"HandshakeTimeToComplete":1500000000,"CustomerMessage":"Handshake completed successfully"}`

	complete := &HandshakeComplete{}
	err := complete.Parse([]byte(payload))
	require.NoError(t, err)

	assert.Equal(t, int64(1500000000), complete.HandshakeTimeToComplete)
	assert.Equal(t, "Handshake completed successfully", complete.CustomerMessage)
}

func TestProcessHandshake_SessionType(t *testing.T) {
	payload := `{"AgentVersion":"3.2.2016.0","RequestedClientActions":[{"ActionType":"SessionType","ActionParameters":{"SessionType":"Port","Properties":{"portNumber":"22"}}}]}`

	processor := NewHandshakeProcessor("1.0.0")
	resp, err := processor.ProcessHandshake([]byte(payload))
	require.NoError(t, err)

	// Parse response to verify
	respData, err := resp.Build()
	require.NoError(t, err)

	assert.Contains(t, string(respData), `"ActionStatus":1`) // Success
	assert.Equal(t, "Port", processor.SessionType())
}

func TestProcessHandshake_UnsupportedKMS(t *testing.T) {
	payload := `{"AgentVersion":"3.2.2016.0","RequestedClientActions":[{"ActionType":"KMSEncryption","ActionParameters":{"KMSKeyId":"arn:aws:kms:us-east-1:123456789012:key/test"}}]}`

	processor := NewHandshakeProcessor("1.0.0")
	resp, err := processor.ProcessHandshake([]byte(payload))
	require.NoError(t, err)

	respData, err := resp.Build()
	require.NoError(t, err)

	// KMS should be marked as unsupported since we don't implement it
	assert.Contains(t, string(respData), `"ActionStatus":3`) // Unsupported
}

func TestProcessHandshake_MultipleActions(t *testing.T) {
	payload := `{"AgentVersion":"3.2.2016.0","RequestedClientActions":[{"ActionType":"SessionType","ActionParameters":{"SessionType":"Port","Properties":{}}},{"ActionType":"KMSEncryption","ActionParameters":{"KMSKeyId":"test"}}]}`

	processor := NewHandshakeProcessor("1.0.0")
	resp, err := processor.ProcessHandshake([]byte(payload))
	require.NoError(t, err)

	assert.Len(t, resp.ProcessedClientActions, 2)
}

func TestSessionTypeParameters_Parse(t *testing.T) {
	params := `{"SessionType":"Port","Properties":{"portNumber":"22","localPortNumber":"2222"}}`

	stp := &SessionTypeParameters{}
	err := stp.Parse([]byte(params))
	require.NoError(t, err)

	assert.Equal(t, "Port", stp.SessionType)
	assert.Equal(t, "22", stp.Properties["portNumber"])
	assert.Equal(t, "2222", stp.Properties["localPortNumber"])
}
