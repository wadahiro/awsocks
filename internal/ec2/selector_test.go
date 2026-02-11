package ec2

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectInstance_SingleInstance(t *testing.T) {
	instances := []Instance{
		{ID: "i-12345", Name: "my-server", State: "running"},
	}

	var output bytes.Buffer
	selector := NewSelectorWithIO(strings.NewReader(""), &output)

	selected, err := selector.SelectInstance(instances)

	require.NoError(t, err)
	assert.Equal(t, "i-12345", selected.ID)
	assert.Equal(t, "my-server", selected.Name)
	// No prompt should be shown for single instance
	assert.Empty(t, output.String())
}

func TestSelectInstance_MultipleInstances_DefaultSelection(t *testing.T) {
	instances := []Instance{
		{ID: "i-11111", Name: "web-1", State: "running"},
		{ID: "i-22222", Name: "web-2", State: "running"},
	}

	var output bytes.Buffer
	selector := NewSelectorWithIO(strings.NewReader("\n"), &output)

	selected, err := selector.SelectInstance(instances)

	require.NoError(t, err)
	assert.Equal(t, "i-11111", selected.ID)
	assert.Equal(t, "web-1", selected.Name)
	assert.Contains(t, output.String(), "Found 2 instances")
	assert.Contains(t, output.String(), "[1] web-1")
	assert.Contains(t, output.String(), "[2] web-2")
}

func TestSelectInstance_MultipleInstances_ExplicitSelection(t *testing.T) {
	instances := []Instance{
		{ID: "i-11111", Name: "web-1", State: "running"},
		{ID: "i-22222", Name: "web-2", State: "running"},
		{ID: "i-33333", Name: "web-3", State: "running"},
	}

	var output bytes.Buffer
	selector := NewSelectorWithIO(strings.NewReader("2\n"), &output)

	selected, err := selector.SelectInstance(instances)

	require.NoError(t, err)
	assert.Equal(t, "i-22222", selected.ID)
	assert.Equal(t, "web-2", selected.Name)
	assert.Contains(t, output.String(), "Selected: web-2")
}

func TestSelectInstance_InvalidSelection(t *testing.T) {
	instances := []Instance{
		{ID: "i-11111", Name: "web-1", State: "running"},
		{ID: "i-22222", Name: "web-2", State: "running"},
	}

	var output bytes.Buffer
	selector := NewSelectorWithIO(strings.NewReader("5\n"), &output)

	selected, err := selector.SelectInstance(instances)

	assert.Error(t, err)
	assert.Nil(t, selected)
	assert.Contains(t, err.Error(), "invalid selection")
}

func TestSelectInstance_NonNumericSelection(t *testing.T) {
	instances := []Instance{
		{ID: "i-11111", Name: "web-1", State: "running"},
		{ID: "i-22222", Name: "web-2", State: "running"},
	}

	var output bytes.Buffer
	selector := NewSelectorWithIO(strings.NewReader("abc\n"), &output)

	selected, err := selector.SelectInstance(instances)

	assert.Error(t, err)
	assert.Nil(t, selected)
	assert.Contains(t, err.Error(), "invalid selection")
}

func TestSelectInstance_EmptyList(t *testing.T) {
	var output bytes.Buffer
	selector := NewSelectorWithIO(strings.NewReader(""), &output)

	selected, err := selector.SelectInstance([]Instance{})

	assert.Error(t, err)
	assert.Nil(t, selected)
	assert.Contains(t, err.Error(), "no instances to select from")
}

func TestSelectInstance_ZeroSelection(t *testing.T) {
	instances := []Instance{
		{ID: "i-11111", Name: "web-1", State: "running"},
		{ID: "i-22222", Name: "web-2", State: "running"},
	}

	var output bytes.Buffer
	selector := NewSelectorWithIO(strings.NewReader("0\n"), &output)

	selected, err := selector.SelectInstance(instances)

	assert.Error(t, err)
	assert.Nil(t, selected)
	assert.Contains(t, err.Error(), "invalid selection")
}
