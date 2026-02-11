// Package credentials handles AWS credential management and auto-refresh
package credentials

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/fsnotify/fsnotify"
	"github.com/wadahiro/awsocks/internal/log"
)

var logger = log.For(log.ComponentCredentials)

// Provider watches AWS credential files and provides auto-refresh
type Provider struct {
	profile         string
	region          string
	awsDir          string
	refreshCh       chan aws.Credentials
	watcher         *fsnotify.Watcher
	lastCreds       aws.Credentials
	lastCredsTime   time.Time
	mu              sync.RWMutex
	retryInterval   time.Duration
	expiryBuffer    time.Duration
}

// NewProvider creates a new credential provider
func NewProvider(profile, region string) *Provider {
	homeDir, _ := os.UserHomeDir()
	return &Provider{
		profile:       profile,
		region:        region,
		awsDir:        filepath.Join(homeDir, ".aws"),
		refreshCh:     make(chan aws.Credentials, 1),
		retryInterval: 10 * time.Second, // Retry interval when credentials fail to load
		expiryBuffer:  5 * time.Minute,  // Refresh credentials this long before expiry
	}
}

// Start begins watching for credential changes
func (p *Provider) Start(ctx context.Context) error {
	// Initial credential load
	if err := p.loadAndSend(ctx); err != nil {
		return fmt.Errorf("failed to load initial credentials: %w", err)
	}

	// Setup file watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create watcher: %w", err)
	}
	p.watcher = watcher

	// Watch credential files
	credentialsFile := filepath.Join(p.awsDir, "credentials")
	configFile := filepath.Join(p.awsDir, "config")

	if _, err := os.Stat(credentialsFile); err == nil {
		if err := watcher.Add(credentialsFile); err != nil {
			logger.Warn("failed to watch credentials file", "error", err)
		}
	}

	if _, err := os.Stat(configFile); err == nil {
		if err := watcher.Add(configFile); err != nil {
			logger.Warn("failed to watch config file", "error", err)
		}
	}

	// Watch SSO cache directory for SSO credential updates
	ssoCacheDir := filepath.Join(p.awsDir, "sso", "cache")
	if _, err := os.Stat(ssoCacheDir); err == nil {
		if err := watcher.Add(ssoCacheDir); err != nil {
			logger.Warn("failed to watch SSO cache", "error", err)
		}
	}

	// Start watcher goroutine
	go p.watchLoop(ctx)

	return nil
}

func (p *Provider) watchLoop(ctx context.Context) {
	// Debounce timer to avoid rapid reloads
	var debounceTimer *time.Timer
	debounceDelay := 500 * time.Millisecond

	// Expiry timer - scheduled based on credential expiration
	var expiryTimer *time.Timer

	// Retry timer for failed loads
	var retryTimer *time.Timer

	// Schedule initial expiry timer based on loaded credentials
	p.scheduleExpiryRefresh(ctx, &expiryTimer, &retryTimer)

	for {
		select {
		case <-ctx.Done():
			p.watcher.Close()
			if expiryTimer != nil {
				expiryTimer.Stop()
			}
			if retryTimer != nil {
				retryTimer.Stop()
			}
			return

		case event, ok := <-p.watcher.Events:
			if !ok {
				return
			}

			// Only react to write events
			if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
				// Debounce: reset timer on each event
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.AfterFunc(debounceDelay, func() {
					logger.Debug("Detected change, reloading...", "file", event.Name)
					if err := p.loadAndSend(ctx); err != nil {
						logger.Error("Failed to reload credentials", "error", err)
						p.scheduleRetry(ctx, &retryTimer)
					} else {
						logger.Info("Credentials reloaded successfully")
						p.scheduleExpiryRefresh(ctx, &expiryTimer, &retryTimer)
					}
				})
			}

		case err, ok := <-p.watcher.Errors:
			if !ok {
				return
			}
			logger.Error("Watcher error", "error", err)
		}
	}
}

// scheduleExpiryRefresh schedules credential refresh based on expiration time
func (p *Provider) scheduleExpiryRefresh(ctx context.Context, expiryTimer **time.Timer, retryTimer **time.Timer) {
	// Cancel existing timer
	if *expiryTimer != nil {
		(*expiryTimer).Stop()
	}

	p.mu.RLock()
	expires := p.lastCreds.Expires
	p.mu.RUnlock()

	// No expiration (static credentials) - no need to schedule
	if expires.IsZero() {
		return
	}

	// Calculate remaining time until expiration
	remaining := time.Until(expires)

	// If already expired, don't schedule (let the next operation fail and trigger refresh)
	if remaining <= 0 {
		return
	}

	// Use dynamic buffer: refresh at 80% of remaining time, but at least 30 seconds before expiry
	// This handles short-lived credentials (e.g., 2-5 minutes) properly
	buffer := remaining / 5 // 20% of remaining time
	if buffer < 30*time.Second {
		buffer = 30 * time.Second
	}
	if buffer > p.expiryBuffer {
		buffer = p.expiryBuffer
	}

	delay := remaining - buffer
	if delay < 10*time.Second {
		delay = 10 * time.Second // Minimum 10 second delay to avoid tight loops
	}

	*expiryTimer = time.AfterFunc(delay, func() {
		logger.Debug("Credential expiry approaching, refreshing...")
		if err := p.loadAndSend(ctx); err != nil {
			logger.Error("Scheduled refresh failed", "error", err)
			p.scheduleRetry(ctx, retryTimer)
		} else {
			logger.Debug("Scheduled refresh successful")
			p.scheduleExpiryRefresh(ctx, expiryTimer, retryTimer)
		}
	})
}

// scheduleRetry schedules a retry for failed credential loads
func (p *Provider) scheduleRetry(ctx context.Context, retryTimer **time.Timer) {
	if *retryTimer != nil {
		(*retryTimer).Stop()
	}
	*retryTimer = time.AfterFunc(p.retryInterval, func() {
		logger.Debug("Retrying credential load...")
		if err := p.loadAndSend(ctx); err != nil {
			logger.Warn("Retry failed, will retry", "error", err, "interval", p.retryInterval)
			p.scheduleRetry(ctx, retryTimer)
		} else {
			logger.Debug("Retry successful")
		}
	})
}

func (p *Provider) loadAndSend(ctx context.Context) error {
	opts := []func(*config.LoadOptions) error{}

	if p.profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(p.profile))
	}
	if p.region != "" {
		opts = append(opts, config.WithRegion(p.region))
	}

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("failed to retrieve credentials: %w", err)
	}

	// Store last credentials for expiry tracking
	p.mu.Lock()
	p.lastCreds = creds
	p.lastCredsTime = time.Now()
	p.mu.Unlock()

	// Log expiration info
	if !creds.Expires.IsZero() {
		remaining := time.Until(creds.Expires)
		logger.Info("Loaded credentials", "expiresIn", remaining.Round(time.Second))
	} else {
		logger.Info("Loaded static credentials (no expiration)")
	}

	// Send to channel (non-blocking)
	select {
	case p.refreshCh <- creds:
	default:
		// Channel full, replace with new credentials
		select {
		case <-p.refreshCh:
		default:
		}
		p.refreshCh <- creds
	}

	return nil
}

// RefreshChannel returns the channel that receives updated credentials
func (p *Provider) RefreshChannel() <-chan aws.Credentials {
	return p.refreshCh
}

// GetCredentials retrieves current credentials
func (p *Provider) GetCredentials(ctx context.Context) (aws.Credentials, error) {
	opts := []func(*config.LoadOptions) error{}

	if p.profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(p.profile))
	}
	if p.region != "" {
		opts = append(opts, config.WithRegion(p.region))
	}

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return cfg.Credentials.Retrieve(ctx)
}

// Stop stops the credential provider
func (p *Provider) Stop() {
	if p.watcher != nil {
		p.watcher.Close()
	}
}

// RequestRefresh triggers an immediate credential refresh
// This can be called when VM agent reports credential errors
func (p *Provider) RequestRefresh(ctx context.Context) {
	logger.Debug("Refresh requested by agent")
	go func() {
		if err := p.loadAndSend(ctx); err != nil {
			logger.Error("Requested refresh failed", "error", err)
		}
	}()
}

// GetLastCredentials returns the last loaded credentials
func (p *Provider) GetLastCredentials() aws.Credentials {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastCreds
}
