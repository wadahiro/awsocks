package credentials

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
)

func TestNewProvider(t *testing.T) {
	p := NewProvider("test-profile", "us-west-2")

	assert.Equal(t, "test-profile", p.profile)
	assert.Equal(t, "us-west-2", p.region)
	assert.NotNil(t, p.refreshCh)
	assert.Equal(t, 10*time.Second, p.retryInterval)
	assert.Equal(t, 5*time.Minute, p.expiryBuffer)
}

func TestRefreshChannel(t *testing.T) {
	p := NewProvider("", "")

	ch := p.RefreshChannel()
	assert.NotNil(t, ch)

	// Test that channel is readable type
	var readOnly <-chan aws.Credentials = ch
	assert.NotNil(t, readOnly)
}

func TestGetLastCredentials(t *testing.T) {
	p := NewProvider("", "")

	// Initially empty
	creds := p.GetLastCredentials()
	assert.Empty(t, creds.AccessKeyID)

	// Set credentials
	p.mu.Lock()
	p.lastCreds = aws.Credentials{
		AccessKeyID:     "AKIATEST",
		SecretAccessKey: "secret",
		SessionToken:    "token",
	}
	p.mu.Unlock()

	// Get credentials
	creds = p.GetLastCredentials()
	assert.Equal(t, "AKIATEST", creds.AccessKeyID)
	assert.Equal(t, "secret", creds.SecretAccessKey)
	assert.Equal(t, "token", creds.SessionToken)
}

func TestProvider_Stop(t *testing.T) {
	p := NewProvider("", "")

	// Stop should not panic even without watcher
	p.Stop()
}
