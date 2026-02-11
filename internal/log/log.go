// Package log provides structured logging with color support for CLI applications.
package log

import (
	"io"
	"log/slog"
	"os"
	"sync/atomic"
)

// Component represents a known component name for logging.
type Component string

const (
	ComponentAgent       Component = "agent"
	ComponentManager     Component = "manager"
	ComponentCredentials Component = "credentials"
	ComponentProxy       Component = "proxy"
	ComponentSSM         Component = "ssm"
	ComponentDataChannel Component = "datachannel"
	ComponentWebSocket   Component = "websocket"
	ComponentEC2         Component = "ec2"
	ComponentRouting     Component = "routing"
)

var defaultLogger atomic.Pointer[slog.Logger]

func init() {
	logger := New(os.Stderr, nil)
	defaultLogger.Store(logger)
}

// New creates a new logger with the given writer and options.
func New(w io.Writer, opts *HandlerOptions) *slog.Logger {
	if opts == nil {
		// Use DynamicLevel to allow level changes at runtime
		opts = &HandlerOptions{Level: DynamicLevel{}}
	}
	handler := NewColorHandler(w, opts)
	return slog.New(handler)
}

// For returns a logger for the specified component.
func For(component Component) *slog.Logger {
	return Default().With("component", string(component))
}

// Default returns the default logger.
func Default() *slog.Logger {
	return defaultLogger.Load()
}

// SetDefault sets the default logger.
func SetDefault(logger *slog.Logger) {
	defaultLogger.Store(logger)
}
