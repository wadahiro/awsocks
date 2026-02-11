package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/ec2"
)

func TestFormatInstanceList(t *testing.T) {
	instances := []ec2.Instance{
		{ID: "i-0abc123", Name: "bastion-prod", State: "running"},
		{ID: "i-0def456", Name: "bastion-dev", State: "stopped"},
	}

	var output bytes.Buffer
	FormatInstanceList(&output, instances)

	result := output.String()
	assert.Contains(t, result, "Available instances:")
	assert.Contains(t, result, "1.")
	assert.Contains(t, result, "i-0abc123")
	assert.Contains(t, result, "bastion-prod")
	assert.Contains(t, result, "running")
	assert.Contains(t, result, "2.")
	assert.Contains(t, result, "i-0def456")
	assert.Contains(t, result, "bastion-dev")
	assert.Contains(t, result, "stopped")
}

func TestParseSelection_Valid(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		max       int
		expected  int
	}{
		{
			name:     "single digit",
			input:    "1",
			max:      3,
			expected: 1,
		},
		{
			name:     "with newline",
			input:    "2\n",
			max:      3,
			expected: 2,
		},
		{
			name:     "with whitespace",
			input:    "  3  \n",
			max:      3,
			expected: 3,
		},
		{
			name:     "empty defaults to 1",
			input:    "\n",
			max:      3,
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseSelection(tt.input, tt.max)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseSelection_OutOfRange(t *testing.T) {
	tests := []struct {
		name  string
		input string
		max   int
	}{
		{
			name:  "zero",
			input: "0",
			max:   3,
		},
		{
			name:  "negative",
			input: "-1",
			max:   3,
		},
		{
			name:  "too large",
			input: "4",
			max:   3,
		},
		{
			name:  "much too large",
			input: "100",
			max:   3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSelection(tt.input, tt.max)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "out of range")
		})
	}
}

func TestParseSelection_InvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "letters",
			input: "abc",
		},
		{
			name:  "special chars",
			input: "!@#",
		},
		{
			name:  "mixed",
			input: "1a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSelection(tt.input, 3)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid")
		})
	}
}

func TestInstanceSelector_SelectInstance(t *testing.T) {
	instances := []ec2.Instance{
		{ID: "i-0abc123", Name: "bastion-prod", State: "running"},
		{ID: "i-0def456", Name: "bastion-dev", State: "stopped"},
	}

	tests := []struct {
		name       string
		input      string
		expectedID string
	}{
		{
			name:       "select first",
			input:      "1\n",
			expectedID: "i-0abc123",
		},
		{
			name:       "select second",
			input:      "2\n",
			expectedID: "i-0def456",
		},
		{
			name:       "empty selects first",
			input:      "\n",
			expectedID: "i-0abc123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			selector := NewInstanceSelector(strings.NewReader(tt.input), &output)

			selected, err := selector.SelectInstance(instances)
			require.NoError(t, err)
			assert.Equal(t, tt.expectedID, selected.ID)
		})
	}
}

func TestInstanceSelector_SingleInstance(t *testing.T) {
	instances := []ec2.Instance{
		{ID: "i-only", Name: "only-instance", State: "running"},
	}

	var output bytes.Buffer
	selector := NewInstanceSelector(strings.NewReader(""), &output)

	selected, err := selector.SelectInstance(instances)
	require.NoError(t, err)
	assert.Equal(t, "i-only", selected.ID)

	// Should not prompt for selection
	assert.NotContains(t, output.String(), "Select")
}

func TestInstanceSelector_EmptyList(t *testing.T) {
	var output bytes.Buffer
	selector := NewInstanceSelector(strings.NewReader(""), &output)

	_, err := selector.SelectInstance(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no instances")
}

func TestProfileSelector_SelectProfile(t *testing.T) {
	profiles := []string{"work", "client", "personal"}

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "select first",
			input:    "1\n",
			expected: "work",
		},
		{
			name:     "select second",
			input:    "2\n",
			expected: "client",
		},
		{
			name:     "select third",
			input:    "3\n",
			expected: "personal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			selector := NewProfileSelector(strings.NewReader(tt.input), &output)

			selected, err := selector.SelectProfile(profiles)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, selected)
		})
	}
}

func TestFormatProfileList(t *testing.T) {
	profiles := []string{"work", "client"}

	var output bytes.Buffer
	FormatProfileList(&output, profiles)

	result := output.String()
	assert.Contains(t, result, "Available profiles:")
	assert.Contains(t, result, "1.")
	assert.Contains(t, result, "work")
	assert.Contains(t, result, "2.")
	assert.Contains(t, result, "client")
}
