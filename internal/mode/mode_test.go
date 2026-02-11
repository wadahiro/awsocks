package mode

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExecutionMode_String(t *testing.T) {
	tests := []struct {
		mode     ExecutionMode
		expected string
	}{
		{ModeAuto, "auto"},
		{ModeVM, "vm"},
		{ModeDirect, "direct"},
		{ExecutionMode(99), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.mode.String())
		})
	}
}

func TestParseMode(t *testing.T) {
	tests := []struct {
		input    string
		expected ExecutionMode
	}{
		{"vm", ModeVM},
		{"direct", ModeDirect},
		{"auto", ModeAuto},
		{"", ModeAuto},
		{"invalid", ModeAuto},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, ParseMode(tt.input))
		})
	}
}

func TestSelectMode(t *testing.T) {
	tests := []struct {
		name      string
		requested ExecutionMode
		expected  ExecutionMode
	}{
		{
			name:      "auto defaults to direct",
			requested: ModeAuto,
			expected:  ModeDirect,
		},
		{
			name:      "direct stays direct",
			requested: ModeDirect,
			expected:  ModeDirect,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, SelectMode(tt.requested))
		})
	}
}

func TestSelectMode_VMOnMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		// On macOS, VM mode should be allowed
		assert.Equal(t, ModeVM, SelectMode(ModeVM))
	} else {
		// On other OS, VM mode should fall back to direct
		assert.Equal(t, ModeDirect, SelectMode(ModeVM))
	}
}

func TestIsVMSupported(t *testing.T) {
	expected := runtime.GOOS == "darwin"
	assert.Equal(t, expected, IsVMSupported())
}
