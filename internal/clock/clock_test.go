package clock

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRealClock_Now(t *testing.T) {
	c := RealClock{}
	before := time.Now()
	now := c.Now()
	after := time.Now()

	assert.True(t, !now.Before(before), "Now should be >= before")
	assert.True(t, !now.After(after), "Now should be <= after")
}

func TestMockClock_Now(t *testing.T) {
	fixed := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	c := NewMockClock(fixed)

	assert.Equal(t, fixed, c.Now())
}

func TestMockClock_Set(t *testing.T) {
	initial := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	c := NewMockClock(initial)

	newTime := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	c.Set(newTime)

	assert.Equal(t, newTime, c.Now())
}

func TestMockClock_Advance(t *testing.T) {
	initial := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	c := NewMockClock(initial)

	c.Advance(1 * time.Hour)

	expected := initial.Add(1 * time.Hour)
	assert.Equal(t, expected, c.Now())
}

func TestMockClock_AfterFunc(t *testing.T) {
	initial := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	c := NewMockClock(initial)

	var called bool
	var mu sync.Mutex

	c.AfterFunc(5*time.Minute, func() {
		mu.Lock()
		called = true
		mu.Unlock()
	})

	// Should not be called yet
	mu.Lock()
	assert.False(t, called)
	mu.Unlock()

	// Advance but not enough
	c.Advance(3 * time.Minute)
	mu.Lock()
	assert.False(t, called)
	mu.Unlock()

	// Advance past deadline
	c.Advance(3 * time.Minute)
	mu.Lock()
	assert.True(t, called)
	mu.Unlock()
}

func TestMockClock_AfterFunc_Stop(t *testing.T) {
	initial := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	c := NewMockClock(initial)

	var called bool
	var mu sync.Mutex

	timer := c.AfterFunc(5*time.Minute, func() {
		mu.Lock()
		called = true
		mu.Unlock()
	})

	// Stop the timer
	stopped := timer.Stop()
	assert.True(t, stopped)

	// Advance past deadline
	c.Advance(10 * time.Minute)

	// Should not be called because timer was stopped
	mu.Lock()
	assert.False(t, called)
	mu.Unlock()
}

func TestMockClock_AfterFunc_StopTwice(t *testing.T) {
	initial := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	c := NewMockClock(initial)

	timer := c.AfterFunc(5*time.Minute, func() {})

	stopped1 := timer.Stop()
	stopped2 := timer.Stop()

	assert.True(t, stopped1)
	assert.False(t, stopped2)
}

func TestMockClock_MultipleTimers(t *testing.T) {
	initial := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	c := NewMockClock(initial)

	var order []int
	var mu sync.Mutex

	c.AfterFunc(3*time.Minute, func() {
		mu.Lock()
		order = append(order, 1)
		mu.Unlock()
	})

	c.AfterFunc(1*time.Minute, func() {
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
	})

	c.AfterFunc(2*time.Minute, func() {
		mu.Lock()
		order = append(order, 3)
		mu.Unlock()
	})

	c.Advance(5 * time.Minute)

	mu.Lock()
	assert.Len(t, order, 3)
	mu.Unlock()
}
