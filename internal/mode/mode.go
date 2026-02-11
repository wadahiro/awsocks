// Package mode handles execution mode selection (VM vs Direct)
package mode

import "runtime"

// ExecutionMode defines how the proxy operates
type ExecutionMode int

const (
	// ModeAuto automatically selects based on OS
	ModeAuto ExecutionMode = iota
	// ModeVM forces VM mode (macOS only)
	ModeVM
	// ModeDirect forces direct mode (no VM)
	ModeDirect
)

// String returns the mode name
func (m ExecutionMode) String() string {
	switch m {
	case ModeAuto:
		return "auto"
	case ModeVM:
		return "vm"
	case ModeDirect:
		return "direct"
	default:
		return "unknown"
	}
}

// ParseMode converts a string to ExecutionMode
func ParseMode(s string) ExecutionMode {
	switch s {
	case "vm":
		return ModeVM
	case "direct":
		return ModeDirect
	default:
		return ModeAuto
	}
}

// SelectMode determines the execution mode based on OS and user preference
// Default is always Direct mode (no VM)
// VM mode must be explicitly requested with --mode vm
func SelectMode(requested ExecutionMode) ExecutionMode {
	if requested == ModeVM {
		if runtime.GOOS != "darwin" {
			// VM mode is only supported on macOS
			return ModeDirect
		}
		return ModeVM
	}
	// Default for all OS: direct mode
	return ModeDirect
}

// IsVMSupported returns true if VM mode is supported on the current OS
func IsVMSupported() bool {
	return runtime.GOOS == "darwin"
}
