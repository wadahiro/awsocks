package log

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestColorHandler(t *testing.T) {
	buf := &bytes.Buffer{}

	opts := &HandlerOptions{Level: slog.LevelDebug}
	handler := NewColorHandler(buf, opts)
	logger := slog.New(handler).With("component", "test")

	logger.Debug("debug message", "key", "value")
	logger.Info("info message")
	logger.Warn("warn message")
	logger.Error("error message")

	output := buf.String()

	// Check that all messages are present
	if !strings.Contains(output, "DEBUG") {
		t.Error("Expected DEBUG level in output")
	}
	if !strings.Contains(output, "INFO") {
		t.Error("Expected INFO level in output")
	}
	if !strings.Contains(output, "WARN") {
		t.Error("Expected WARN level in output")
	}
	if !strings.Contains(output, "ERROR") {
		t.Error("Expected ERROR level in output")
	}

	// Check component
	if !strings.Contains(output, "[test]") {
		t.Error("Expected [test] component in output")
	}

	// Check messages
	if !strings.Contains(output, "debug message") {
		t.Error("Expected 'debug message' in output")
	}
	if !strings.Contains(output, "key=value") {
		t.Error("Expected 'key=value' in output")
	}
}

func TestForComponent(t *testing.T) {
	buf := &bytes.Buffer{}

	opts := &HandlerOptions{Level: slog.LevelInfo}
	handler := NewColorHandler(buf, opts)
	SetDefault(slog.New(handler))

	logger := For(ComponentManager)
	logger.Info("test message")

	output := buf.String()
	if !strings.Contains(output, "[manager]") {
		t.Errorf("Expected [manager] in output, got: %s", output)
	}
}

func TestLevelFiltering(t *testing.T) {
	buf := &bytes.Buffer{}

	opts := &HandlerOptions{Level: slog.LevelWarn}
	handler := NewColorHandler(buf, opts)
	logger := slog.New(handler)

	logger.Debug("debug")
	logger.Info("info")
	logger.Warn("warn")
	logger.Error("error")

	output := buf.String()

	if strings.Contains(output, "debug") {
		t.Error("DEBUG should be filtered out at WARN level")
	}
	if strings.Contains(output, "info") {
		t.Error("INFO should be filtered out at WARN level")
	}
	if !strings.Contains(output, "warn") {
		t.Error("WARN should be present")
	}
	if !strings.Contains(output, "error") {
		t.Error("ERROR should be present")
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input    string
		expected slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"unknown", slog.LevelInfo}, // default
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := ParseLevel(tt.input)
			if result != tt.expected {
				t.Errorf("ParseLevel(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}
