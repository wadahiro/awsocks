package log

import (
	"log/slog"
	"sync/atomic"
)

var currentLevel atomic.Int64

func init() {
	currentLevel.Store(int64(slog.LevelInfo))
}

// GetLevel returns the current log level.
func GetLevel() slog.Level {
	return slog.Level(currentLevel.Load())
}

// DynamicLevel implements slog.Leveler and returns the current level dynamically.
type DynamicLevel struct{}

// Level returns the current log level.
func (d DynamicLevel) Level() slog.Level {
	return GetLevel()
}

// SetLevel sets the log level.
func SetLevel(level slog.Level) {
	currentLevel.Store(int64(level))
}

// SetVerbose enables or disables verbose (debug) logging.
func SetVerbose(verbose bool) {
	if verbose {
		SetLevel(slog.LevelDebug)
	} else {
		SetLevel(slog.LevelInfo)
	}
}

// ParseLevel parses a level string and returns the corresponding slog.Level.
// Valid values: "debug", "info", "warn", "error"
func ParseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
