package ec2

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Selector handles interactive instance selection
type Selector struct {
	reader io.Reader
	writer io.Writer
}

// NewSelector creates a new selector with default stdin/stdout
func NewSelector() *Selector {
	return &Selector{
		reader: os.Stdin,
		writer: os.Stdout,
	}
}

// NewSelectorWithIO creates a new selector with custom IO
func NewSelectorWithIO(reader io.Reader, writer io.Writer) *Selector {
	return &Selector{
		reader: reader,
		writer: writer,
	}
}

// SelectInstance prompts user to select from multiple instances
func (s *Selector) SelectInstance(instances []Instance) (*Instance, error) {
	if len(instances) == 0 {
		return nil, fmt.Errorf("no instances to select from")
	}

	if len(instances) == 1 {
		return &instances[0], nil
	}

	fmt.Fprintf(s.writer, "\nFound %d instances:\n", len(instances))
	fmt.Fprintln(s.writer, "─────────────────────────────────────────────────")
	for i, inst := range instances {
		fmt.Fprintf(s.writer, "  [%d] %s (%s)\n", i+1, inst.Name, inst.ID)
	}
	fmt.Fprintln(s.writer, "─────────────────────────────────────────────────")
	fmt.Fprint(s.writer, "Select instance [1]: ")

	reader := bufio.NewReader(s.reader)
	input, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	input = strings.TrimSpace(input)
	if input == "" {
		input = "1"
	}

	idx, err := strconv.Atoi(input)
	if err != nil || idx < 1 || idx > len(instances) {
		return nil, fmt.Errorf("invalid selection: %s", input)
	}

	selected := &instances[idx-1]
	fmt.Fprintf(s.writer, "Selected: %s (%s)\n\n", selected.Name, selected.ID)
	return selected, nil
}
