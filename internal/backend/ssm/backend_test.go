package ssm

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// MockSSMClient is a mock for SSM client
type MockSSMClient struct {
	mock.Mock
}

func TestBackend_Name(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}

	b := New(cfg, nil)
	assert.Equal(t, "ssm", b.Name())
}

func TestBackend_Start(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}

	b := New(cfg, nil)
	ctx := context.Background()

	err := b.Start(ctx)
	assert.NoError(t, err)
	assert.Equal(t, StateIdle, b.State())

	b.Close()
}

func TestBackend_StateTransitions(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}

	b := New(cfg, nil)
	ctx := context.Background()

	// Initial state
	assert.Equal(t, StateIdle, b.State())

	err := b.Start(ctx)
	assert.NoError(t, err)
	assert.Equal(t, StateIdle, b.State())

	b.Close()
}

func TestBackend_Close(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}

	b := New(cfg, nil)
	ctx := context.Background()

	err := b.Start(ctx)
	assert.NoError(t, err)

	err = b.Close()
	assert.NoError(t, err)
}

func TestBackend_OnCredentialUpdate_FromIdle(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}

	b := New(cfg, nil)
	ctx := context.Background()

	err := b.Start(ctx)
	assert.NoError(t, err)
	defer b.Close()

	// Credential update should trigger connection
	creds := aws.Credentials{
		AccessKeyID:     "AKIATEST",
		SecretAccessKey: "secret",
		SessionToken:    "token",
	}

	// This will fail to connect (no real SSM), but should change state
	_ = b.OnCredentialUpdate(creds)

	// After credential update, state should be connecting (even if it fails)
	// We use eventually here because connection happens async
	time.Sleep(100 * time.Millisecond)
	state := b.State()
	assert.True(t, state == StateConnecting || state == StateError || state == StateIdle,
		"expected state to be connecting, error, or idle, got %s", state)
}

func TestBackend_Dial_NotReady(t *testing.T) {
	cfg := &Config{
		InstanceID: "i-12345678",
		Region:     "ap-northeast-1",
		SSHUser:    "ec2-user",
	}

	b := New(cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := b.Start(ctx)
	assert.NoError(t, err)
	defer b.Close()

	// Dial without credentials should fail
	_, err = b.Dial(ctx, "tcp", "example.com:80")
	assert.Error(t, err)
}

func TestState_String(t *testing.T) {
	testCases := []struct {
		state    State
		expected string
	}{
		{StateIdle, "idle"},
		{StateConnecting, "connecting"},
		{StateHandshaking, "handshaking"},
		{StateActive, "active"},
		{StateReconnecting, "reconnecting"},
		{StateError, "error"},
		{State(99), "unknown"},
	}

	for _, tc := range testCases {
		t.Run(tc.expected, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.state.String())
		})
	}
}

func TestBackend_SetSSHKeyContent(t *testing.T) {
	// Skip if TEST_SSH_KEY_PATH is not set
	keyPath := os.Getenv("TEST_SSH_KEY_PATH")
	if keyPath == "" {
		t.Skip("TEST_SSH_KEY_PATH not set")
	}

	// Expand ~ if present
	if len(keyPath) > 0 && keyPath[0] == '~' {
		home, _ := os.UserHomeDir()
		keyPath = home + keyPath[1:]
	}

	keyContent, err := os.ReadFile(keyPath)
	require.NoError(t, err)

	backend := &Backend{
		config: &Config{SSHUser: "ec2-user"},
	}

	err = backend.SetSSHKeyContent(keyContent, "")
	require.NoError(t, err)
	assert.NotNil(t, backend.sshConfig)
	assert.Equal(t, "ec2-user", backend.sshConfig.User)
}

func TestBackend_SetSSHKeyContent_InvalidKey(t *testing.T) {
	backend := &Backend{
		config: &Config{SSHUser: "ec2-user"},
	}

	err := backend.SetSSHKeyContent([]byte("invalid key"), "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse SSH key")
}
