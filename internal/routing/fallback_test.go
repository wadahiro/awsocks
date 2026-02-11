package routing

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsFallbackableError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "EHOSTUNREACH errno",
			err:      syscall.EHOSTUNREACH,
			expected: true,
		},
		{
			name:     "ENETUNREACH errno",
			err:      syscall.ENETUNREACH,
			expected: true,
		},
		{
			name:     "ECONNREFUSED errno",
			err:      syscall.ECONNREFUSED,
			expected: true,
		},
		{
			name:     "wrapped EHOSTUNREACH",
			err:      fmt.Errorf("dial failed: %w", syscall.EHOSTUNREACH),
			expected: true,
		},
		{
			name:     "no route to host message",
			err:      errors.New("dial tcp 10.0.0.1:443: connect: no route to host"),
			expected: true,
		},
		{
			name:     "network is unreachable message",
			err:      errors.New("connect: network is unreachable"),
			expected: true,
		},
		{
			name:     "host is unreachable message",
			err:      errors.New("connect: host is unreachable"),
			expected: true,
		},
		{
			name:     "connection refused message",
			err:      errors.New("dial tcp 10.0.0.1:443: connection refused"),
			expected: true,
		},
		{
			name:     "connection timeout",
			err:      errors.New("connection timeout"),
			expected: false,
		},
		{
			name:     "generic error",
			err:      errors.New("some other error"),
			expected: false,
		},
		{
			name:     "EOF",
			err:      errors.New("EOF"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsFallbackableError(tt.err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFallbackRoute_DirectMode(t *testing.T) {
	router := NewRouter(&Config{Default: "proxy"})

	tests := []struct {
		name           string
		currentRoute   Route
		expectedFallback Route
	}{
		{
			name:           "proxy -> direct",
			currentRoute:   RouteProxy,
			expectedFallback: RouteDirect,
		},
		{
			name:           "direct -> proxy",
			currentRoute:   RouteDirect,
			expectedFallback: RouteProxy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := router.FallbackRoute(tt.currentRoute)
			assert.Equal(t, tt.expectedFallback, result)
		})
	}
}

func TestFallbackRoute_VMMode(t *testing.T) {
	router := NewRouter(&Config{Default: "proxy"}, WithVMMode())

	tests := []struct {
		name           string
		currentRoute   Route
		expectedFallback Route
	}{
		{
			name:           "proxy -> vm-direct",
			currentRoute:   RouteProxy,
			expectedFallback: RouteVMDirect,
		},
		{
			name:           "vm-direct -> proxy",
			currentRoute:   RouteVMDirect,
			expectedFallback: RouteProxy,
		},
		{
			name:           "direct -> no fallback (explicit config)",
			currentRoute:   RouteDirect,
			expectedFallback: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := router.FallbackRoute(tt.currentRoute)
			assert.Equal(t, tt.expectedFallback, result)
		})
	}
}
