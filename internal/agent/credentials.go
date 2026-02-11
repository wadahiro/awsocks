// Package agent implements the VM-side agent components
package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// CredentialCache stores AWS credentials received from the host
// It implements aws.CredentialsProvider interface with expiration handling
type CredentialCache struct {
	mu              sync.RWMutex
	credentials     aws.Credentials
	expireTime      time.Time
	requestRefresh  func() // Callback to request credential refresh from host
	lastRefreshReq  time.Time
	refreshInterval time.Duration
}

// NewCredentialCache creates a new credential cache
func NewCredentialCache() *CredentialCache {
	return &CredentialCache{
		refreshInterval: 30 * time.Second, // Minimum interval between refresh requests
	}
}

// SetRefreshCallback sets the callback function to request credential refresh
func (c *CredentialCache) SetRefreshCallback(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requestRefresh = fn
}

// Update stores new credentials received from host
func (c *CredentialCache) Update(creds aws.Credentials) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.credentials = creds
	c.expireTime = creds.Expires
}

// Retrieve returns cached credentials (implements aws.CredentialsProvider)
// If credentials are expired, it requests a refresh and returns an error
func (c *CredentialCache) Retrieve(ctx context.Context) (aws.Credentials, error) {
	c.mu.RLock()
	creds := c.credentials
	expireTime := c.expireTime
	requestRefresh := c.requestRefresh
	lastRefreshReq := c.lastRefreshReq
	refreshInterval := c.refreshInterval
	c.mu.RUnlock()

	if creds.AccessKeyID == "" {
		c.triggerRefresh(requestRefresh, lastRefreshReq, refreshInterval)
		return aws.Credentials{}, fmt.Errorf("no credentials available")
	}

	// Check if credentials are expired or about to expire
	if !expireTime.IsZero() && time.Now().After(expireTime.Add(-1*time.Minute)) {
		c.triggerRefresh(requestRefresh, lastRefreshReq, refreshInterval)

		// If already expired, return error
		if time.Now().After(expireTime) {
			return aws.Credentials{}, fmt.Errorf("credentials expired at %v", expireTime)
		}
	}

	return creds, nil
}

// triggerRefresh requests credential refresh if enough time has passed
func (c *CredentialCache) triggerRefresh(requestRefresh func(), lastRefreshReq time.Time, refreshInterval time.Duration) {
	if requestRefresh == nil {
		return
	}

	// Rate limit refresh requests
	if time.Since(lastRefreshReq) < refreshInterval {
		return
	}

	c.mu.Lock()
	c.lastRefreshReq = time.Now()
	c.mu.Unlock()

	// Request refresh asynchronously
	go requestRefresh()
}

// IsExpired checks if credentials need refresh
func (c *CredentialCache) IsExpired() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.expireTime.IsZero() {
		return false // No expiration set (static credentials)
	}
	// Return true if within 5 minutes of expiration
	return time.Now().After(c.expireTime.Add(-5 * time.Minute))
}

// IsHardExpired checks if credentials are past expiration (not usable)
func (c *CredentialCache) IsHardExpired() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.expireTime.IsZero() {
		return false
	}
	return time.Now().After(c.expireTime)
}

// HasCredentials returns true if credentials are available
func (c *CredentialCache) HasCredentials() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.credentials.AccessKeyID != ""
}

// GetExpiration returns the credential expiration time
func (c *CredentialCache) GetExpiration() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.expireTime
}

// WaitForCredentials waits until valid credentials are available or context is cancelled
func (c *CredentialCache) WaitForCredentials(ctx context.Context) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if c.HasCredentials() && !c.IsHardExpired() {
				return nil
			}
		}
	}
}
