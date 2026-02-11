package routing

import (
	"errors"
	"strings"
	"syscall"
)

// IsFallbackableError returns true if the error indicates a network
// reachability issue that should trigger fallback to an alternative route.
// This includes "no route to host", "network unreachable", and "connection refused".
func IsFallbackableError(err error) bool {
	if err == nil {
		return false
	}

	// Check for syscall errno
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EHOSTUNREACH, // No route to host
			syscall.ENETUNREACH,  // Network is unreachable
			syscall.ECONNREFUSED: // Connection refused
			return true
		}
	}

	// Check error message for wrapped errors
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "host is unreachable") ||
		strings.Contains(msg, "connection refused")
}
