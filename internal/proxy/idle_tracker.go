package proxy

import (
	"sync"
	"time"

	"github.com/wadahiro/awsocks/internal/clock"
)

// IdleTracker monitors proxy activity and triggers a callback when idle for too long.
type IdleTracker struct {
	timeout   time.Duration
	clock     clock.Clock
	onIdle    func()
	mu        sync.Mutex
	timer     clock.Timer
	suspended bool
}

// NewIdleTracker creates a new idle tracker.
func NewIdleTracker(timeout time.Duration, clk clock.Clock, onIdle func()) *IdleTracker {
	return &IdleTracker{
		timeout: timeout,
		clock:   clk,
		onIdle:  onIdle,
	}
}

// Start begins the idle timer.
func (t *IdleTracker) Start() {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Stop existing timer if any
	if t.timer != nil {
		t.timer.Stop()
	}

	t.timer = t.clock.AfterFunc(t.timeout, func() {
		t.mu.Lock()
		t.suspended = true
		t.mu.Unlock()
		t.onIdle()
	})
}

// Stop cancels the idle timer.
func (t *IdleTracker) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

// Touch resets the idle timer, indicating activity.
func (t *IdleTracker) Touch() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.timer != nil {
		t.timer.Stop()
	}

	t.timer = t.clock.AfterFunc(t.timeout, func() {
		t.mu.Lock()
		t.suspended = true
		t.mu.Unlock()
		t.onIdle()
	})
}

// IsSuspended returns true if the idle timeout has fired and EC2 is suspended.
func (t *IdleTracker) IsSuspended() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.suspended
}

// ClearSuspended resets the suspended state after re-initialization.
func (t *IdleTracker) ClearSuspended() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.suspended = false
}
