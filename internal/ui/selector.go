// Package ui provides interactive user interface components.
package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/wadahiro/awsocks/internal/ec2"
)

// InstanceSelector handles interactive EC2 instance selection.
type InstanceSelector struct {
	reader io.Reader
	writer io.Writer
}

// NewInstanceSelector creates a new InstanceSelector with custom IO.
func NewInstanceSelector(reader io.Reader, writer io.Writer) *InstanceSelector {
	return &InstanceSelector{
		reader: reader,
		writer: writer,
	}
}

// DefaultInstanceSelector creates an InstanceSelector using stdin/stdout.
func DefaultInstanceSelector() *InstanceSelector {
	return &InstanceSelector{
		reader: os.Stdin,
		writer: os.Stdout,
	}
}

// SelectInstance prompts the user to select from multiple instances.
// If there's only one instance, it returns that instance without prompting.
// Returns an error if the list is empty.
func (s *InstanceSelector) SelectInstance(instances []ec2.Instance) (*ec2.Instance, error) {
	if len(instances) == 0 {
		return nil, fmt.Errorf("no instances to select from")
	}

	if len(instances) == 1 {
		return &instances[0], nil
	}

	FormatInstanceList(s.writer, instances)
	fmt.Fprint(s.writer, "Select instance [1]: ")

	reader := bufio.NewReader(s.reader)
	input, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	selection, err := ParseSelection(input, len(instances))
	if err != nil {
		return nil, err
	}

	selected := &instances[selection-1]
	fmt.Fprintf(s.writer, "Selected: %s (%s)\n\n", selected.Name, selected.ID)
	return selected, nil
}

// FormatInstanceList formats a list of instances for display.
func FormatInstanceList(w io.Writer, instances []ec2.Instance) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Available instances:")
	fmt.Fprintln(w, "─────────────────────────────────────────────────")
	for i, inst := range instances {
		fmt.Fprintf(w, "  %d. %s (%s) - %s\n", i+1, inst.ID, inst.Name, inst.State)
	}
	fmt.Fprintln(w, "─────────────────────────────────────────────────")
}

// ProfileSelector handles interactive profile selection.
type ProfileSelector struct {
	reader io.Reader
	writer io.Writer
}

// NewProfileSelector creates a new ProfileSelector with custom IO.
func NewProfileSelector(reader io.Reader, writer io.Writer) *ProfileSelector {
	return &ProfileSelector{
		reader: reader,
		writer: writer,
	}
}

// DefaultProfileSelector creates a ProfileSelector using stdin/stdout.
func DefaultProfileSelector() *ProfileSelector {
	return &ProfileSelector{
		reader: os.Stdin,
		writer: os.Stdout,
	}
}

// SelectProfile prompts the user to select from multiple profiles.
// Returns the selected profile name.
func (s *ProfileSelector) SelectProfile(profiles []string) (string, error) {
	if len(profiles) == 0 {
		return "", fmt.Errorf("no profiles to select from")
	}

	if len(profiles) == 1 {
		return profiles[0], nil
	}

	FormatProfileList(s.writer, profiles)
	fmt.Fprint(s.writer, "Select profile [1]: ")

	reader := bufio.NewReader(s.reader)
	input, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("failed to read input: %w", err)
	}

	selection, err := ParseSelection(input, len(profiles))
	if err != nil {
		return "", err
	}

	selected := profiles[selection-1]
	fmt.Fprintf(s.writer, "Selected: %s\n\n", selected)
	return selected, nil
}

// FormatProfileList formats a list of profile names for display.
func FormatProfileList(w io.Writer, profiles []string) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Available profiles:")
	fmt.Fprintln(w, "─────────────────────────────────────────────────")
	for i, name := range profiles {
		fmt.Fprintf(w, "  %d. %s\n", i+1, name)
	}
	fmt.Fprintln(w, "─────────────────────────────────────────────────")
}

// ParseSelection parses user input for selection.
// Empty input defaults to 1.
// Returns an error if the input is invalid or out of range.
func ParseSelection(input string, max int) (int, error) {
	input = strings.TrimSpace(input)

	// Empty input defaults to 1
	if input == "" {
		return 1, nil
	}

	selection, err := strconv.Atoi(input)
	if err != nil {
		return 0, fmt.Errorf("invalid selection: %s", input)
	}

	if selection < 1 || selection > max {
		return 0, fmt.Errorf("selection out of range: %d (valid: 1-%d)", selection, max)
	}

	return selection, nil
}
