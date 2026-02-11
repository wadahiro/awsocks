package log

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
)

// HandlerOptions configures the ColorHandler.
type HandlerOptions struct {
	Level     slog.Leveler
	AddSource bool
}

// ColorHandler is a slog.Handler that supports colored output for TTY.
type ColorHandler struct {
	opts   HandlerOptions
	attrs  []slog.Attr
	groups []string
	mu     *sync.Mutex
	w      io.Writer
	isTTY  bool
}

// NewColorHandler creates a new ColorHandler.
func NewColorHandler(w io.Writer, opts *HandlerOptions) *ColorHandler {
	h := &ColorHandler{
		w:     w,
		mu:    &sync.Mutex{},
		isTTY: isTerminal(w),
	}
	if opts != nil {
		h.opts = *opts
	}
	if h.opts.Level == nil {
		h.opts.Level = slog.LevelInfo
	}
	return h
}

// isTerminal checks if the writer is a TTY.
func isTerminal(w io.Writer) bool {
	if f, ok := w.(*os.File); ok {
		stat, err := f.Stat()
		if err != nil {
			return false
		}
		return (stat.Mode() & os.ModeCharDevice) != 0
	}
	return false
}

// Enabled reports whether the handler handles records at the given level.
func (h *ColorHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.opts.Level.Level()
}

// Handle handles the Record.
func (h *ColorHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Format timestamp in RFC3339 format
	timeStr := r.Time.Format(time.RFC3339)

	// Get level string and color
	levelStr, levelColor := h.formatLevel(r.Level)

	// Find component attribute
	var component string
	var otherAttrs []slog.Attr

	// Collect pre-set attrs
	for _, a := range h.attrs {
		if a.Key == "component" {
			component = a.Value.String()
		} else {
			otherAttrs = append(otherAttrs, a)
		}
	}

	// Collect record attrs
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "component" {
			component = a.Value.String()
		} else {
			otherAttrs = append(otherAttrs, a)
		}
		return true
	})

	// Build output
	if h.isTTY {
		// Color output
		h.w.Write([]byte(colorGray + timeStr + colorReset + " "))
		h.w.Write([]byte(levelColor + levelStr + colorReset + " "))
		if component != "" {
			h.w.Write([]byte("[" + component + "] "))
		}
		h.w.Write([]byte(r.Message))
	} else {
		// Plain text output
		h.w.Write([]byte(timeStr + " "))
		h.w.Write([]byte(levelStr + " "))
		if component != "" {
			h.w.Write([]byte("[" + component + "] "))
		}
		h.w.Write([]byte(r.Message))
	}

	// Write additional attributes
	for _, a := range otherAttrs {
		h.w.Write([]byte(" " + a.Key + "=" + formatValue(a.Value)))
	}

	h.w.Write([]byte("\n"))

	return nil
}

// formatLevel returns the level string and its color.
func (h *ColorHandler) formatLevel(level slog.Level) (string, string) {
	switch {
	case level >= slog.LevelError:
		return "ERROR", colorRed
	case level >= slog.LevelWarn:
		return "WARN ", colorYellow
	case level >= slog.LevelInfo:
		return "INFO ", colorGreen
	default:
		return "DEBUG", colorCyan
	}
}

// formatValue formats an attribute value as a string.
func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	default:
		return v.String()
	}
}

// WithAttrs returns a new Handler with the given attributes added.
func (h *ColorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newHandler := &ColorHandler{
		opts:   h.opts,
		attrs:  make([]slog.Attr, len(h.attrs)+len(attrs)),
		groups: h.groups,
		mu:     h.mu,
		w:      h.w,
		isTTY:  h.isTTY,
	}
	copy(newHandler.attrs, h.attrs)
	copy(newHandler.attrs[len(h.attrs):], attrs)
	return newHandler
}

// WithGroup returns a new Handler with the given group appended to the receiver's groups.
func (h *ColorHandler) WithGroup(name string) slog.Handler {
	newHandler := &ColorHandler{
		opts:   h.opts,
		attrs:  h.attrs,
		groups: make([]string, len(h.groups)+1),
		mu:     h.mu,
		w:      h.w,
		isTTY:  h.isTTY,
	}
	copy(newHandler.groups, h.groups)
	newHandler.groups[len(h.groups)] = name
	return newHandler
}
