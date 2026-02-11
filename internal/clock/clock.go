package clock

import (
	"sync"
	"time"
)

// Clock abstracts time operations for testing
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer represents a timer that can be stopped
type Timer interface {
	Stop() bool
}

// RealClock uses actual time
type RealClock struct{}

// Now returns the current time
func (RealClock) Now() time.Time {
	return time.Now()
}

// After waits for the duration to elapse and then sends the current time
func (RealClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// AfterFunc waits for the duration to elapse and then calls f
func (RealClock) AfterFunc(d time.Duration, f func()) Timer {
	return time.AfterFunc(d, f)
}

// MockClock for testing
type MockClock struct {
	mu       sync.Mutex
	current  time.Time
	timers   []*mockTimer
	channels []chan time.Time
}

type mockTimer struct {
	deadline time.Time
	f        func()
	stopped  bool
}

func (t *mockTimer) Stop() bool {
	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// NewMockClock creates a new mock clock
func NewMockClock(t time.Time) *MockClock {
	return &MockClock{current: t}
}

// Now returns the mock current time
func (m *MockClock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// After returns a channel that will receive the time after duration
func (m *MockClock) After(d time.Duration) <-chan time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()

	ch := make(chan time.Time, 1)
	m.channels = append(m.channels, ch)
	return ch
}

// AfterFunc registers a function to be called after duration
func (m *MockClock) AfterFunc(d time.Duration, f func()) Timer {
	m.mu.Lock()
	defer m.mu.Unlock()

	timer := &mockTimer{
		deadline: m.current.Add(d),
		f:        f,
	}
	m.timers = append(m.timers, timer)
	return timer
}

// Advance moves the mock clock forward by duration
func (m *MockClock) Advance(d time.Duration) {
	m.mu.Lock()
	m.current = m.current.Add(d)
	current := m.current

	// Fire timers that have expired
	var remaining []*mockTimer
	var toFire []func()
	for _, timer := range m.timers {
		if timer.stopped {
			continue
		}
		if !timer.deadline.After(current) {
			toFire = append(toFire, timer.f)
			timer.stopped = true
		} else {
			remaining = append(remaining, timer)
		}
	}
	m.timers = remaining
	m.mu.Unlock()

	// Fire functions outside of lock to avoid deadlock
	for _, f := range toFire {
		f()
	}
}

// Set sets the mock clock to a specific time
func (m *MockClock) Set(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.current = t
}
