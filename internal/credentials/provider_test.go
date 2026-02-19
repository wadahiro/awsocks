package credentials

import (
	"context"
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

func TestGetConfig_InitiallyNil(t *testing.T) {
	p := NewProvider("", "")

	// Initially nil (before Start/loadAndSend)
	cfg := p.GetConfig()
	assert.Nil(t, cfg)
}

func TestGetConfig_CachedAfterLoadAndSend(t *testing.T) {
	p := NewProvider("", "")

	// Simulate loadAndSend caching a config
	testCfg := aws.Config{
		Region: "ap-northeast-1",
	}
	p.mu.Lock()
	p.lastConfig = &testCfg
	p.mu.Unlock()

	cfg := p.GetConfig()
	assert.NotNil(t, cfg)
	assert.Equal(t, "ap-northeast-1", cfg.Region)
}

func TestGetCredentials_UsesCache(t *testing.T) {
	p := NewProvider("", "")

	// Set cached credentials
	p.mu.Lock()
	p.lastCreds = aws.Credentials{
		AccessKeyID:     "AKIACACHED",
		SecretAccessKey: "cached-secret",
		SessionToken:    "cached-token",
	}
	p.mu.Unlock()

	// GetCredentials should return cached credentials without LoadDefaultConfig
	creds, err := p.GetCredentials(context.TODO())
	assert.NoError(t, err)
	assert.Equal(t, "AKIACACHED", creds.AccessKeyID)
	assert.Equal(t, "cached-secret", creds.SecretAccessKey)
	assert.Equal(t, "cached-token", creds.SessionToken)
}

func TestGetCredentials_EmptyCache(t *testing.T) {
	p := NewProvider("", "")

	// No cached credentials - GetCredentials should fall back to loading
	// This will fail because there's no valid AWS config, but it tests the fallback path
	_, err := p.GetCredentials(context.TODO())
	// Should attempt to load and likely fail (no AWS config in test env)
	// The important thing is it doesn't return empty credentials silently
	assert.Error(t, err)
}

func TestProvider_Stop(t *testing.T) {
	p := NewProvider("", "")

	// Stop should not panic even without watcher
	p.Stop()
}
