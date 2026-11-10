// Package obs holds the small pieces of observability shared by the command
// and the web layer: logger construction and bounded log values.
package obs

import (
	"io"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// NewLogger builds the process logger. level is debug, info, warn or error and
// format is json or text; empty values select info and json, so
// configurations written before logging was configurable keep working.
func NewLogger(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: ParseLevel(level)}
	if strings.EqualFold(format, "text") {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// ParseLevel maps a configured level name to a slog level (default info).
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// Discard returns a logger that drops everything.
func Discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Clip bounds an arbitrary string for logging: control characters become
// spaces and the result is cut to max bytes on a rune boundary.
func Clip(s string, max int) string {
	if len(s) > max {
		s = s[:max]
		for !utf8.ValidString(s) && len(s) > 0 {
			s = s[:len(s)-1]
		}
		s += "…"
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
