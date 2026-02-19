package proxy

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/clock"
)

func TestIdleTracker_TimeoutFires(t *testing.T) {
	mockClock := clock.NewMockClock(time.Now())
	var fired atomic.Bool

	tracker := NewIdleTracker(30*time.Minute, mockClock, func() {
		fired.Store(true)
	})

	tracker.Start()

	// Not yet fired
	assert.False(t, fired.Load())

	// Advance past timeout
	mockClock.Advance(31 * time.Minute)

	assert.True(t, fired.Load())
	assert.True(t, tracker.IsSuspended())
}

func TestIdleTracker_TouchResetsTimer(t *testing.T) {
	mockClock := clock.NewMockClock(time.Now())
	var fired atomic.Bool

	tracker := NewIdleTracker(30*time.Minute, mockClock, func() {
		fired.Store(true)
	})

	tracker.Start()

	// Advance 20 minutes, then touch
	mockClock.Advance(20 * time.Minute)
	assert.False(t, fired.Load())

	tracker.Touch()

	// Advance another 20 minutes (total 40, but only 20 since touch)
	mockClock.Advance(20 * time.Minute)
	assert.False(t, fired.Load())

	// Advance past timeout since touch
	mockClock.Advance(11 * time.Minute)
	assert.True(t, fired.Load())
}

func TestIdleTracker_StopPreventsTimeout(t *testing.T) {
	mockClock := clock.NewMockClock(time.Now())
	var fired atomic.Bool

	tracker := NewIdleTracker(30*time.Minute, mockClock, func() {
		fired.Store(true)
	})

	tracker.Start()

	// Stop the tracker
	tracker.Stop()

	// Advance past timeout
	mockClock.Advance(31 * time.Minute)

	assert.False(t, fired.Load())
}

func TestIdleTracker_IsSuspended_ClearSuspended(t *testing.T) {
	mockClock := clock.NewMockClock(time.Now())

	tracker := NewIdleTracker(30*time.Minute, mockClock, func() {})

	// Initially not suspended
	assert.False(t, tracker.IsSuspended())

	// Start and fire timeout
	tracker.Start()
	mockClock.Advance(31 * time.Minute)

	assert.True(t, tracker.IsSuspended())

	// Clear suspended
	tracker.ClearSuspended()
	assert.False(t, tracker.IsSuspended())
}

func TestIdleTracker_TouchBeforeStart(t *testing.T) {
	mockClock := clock.NewMockClock(time.Now())

	tracker := NewIdleTracker(30*time.Minute, mockClock, func() {})

	// Touch before Start should not panic
	require.NotPanics(t, func() {
		tracker.Touch()
	})
}

func TestIdleTracker_StopBeforeStart(t *testing.T) {
	mockClock := clock.NewMockClock(time.Now())

	tracker := NewIdleTracker(30*time.Minute, mockClock, func() {})

	// Stop before Start should not panic
	require.NotPanics(t, func() {
		tracker.Stop()
	})
}

func TestIdleTracker_MultipleStarts(t *testing.T) {
	mockClock := clock.NewMockClock(time.Now())
	var fireCount atomic.Int32

	tracker := NewIdleTracker(30*time.Minute, mockClock, func() {
		fireCount.Add(1)
	})

	// Start twice should not create duplicate timers
	tracker.Start()
	tracker.Start()

	mockClock.Advance(31 * time.Minute)

	// Should only fire once
	assert.Equal(t, int32(1), fireCount.Load())
}
