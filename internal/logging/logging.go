// Package logging configures cmemlan's logger.
//
// Invariant: op bodies, pairing codes, and the pre-shared key are never logged.
// Everything this hub relays is the user's private memory, so a debug-level body
// dump would pipe exactly the data the project exists to protect into journald.
// Secrets are passed through Redact, which keeps the length and discards the value.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New builds a logger. level is debug|info|warn|error, format is text|json.
// Unknown values fall back to info and text rather than failing: a malformed
// logging flag must never prevent the hub from starting.
func New(level, format string, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Redact renders a secret as its length only. Use it for the PSK, pairing codes,
// bearer tokens, and anything derived from an op body.
func Redact(v string) string {
	if v == "" {
		return "[empty]"
	}
	return fmt.Sprintf("[redacted len=%d]", len(v))
}
