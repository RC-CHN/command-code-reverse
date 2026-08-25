// Package logging initializes the process-wide slog logger.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Init configures slog with the given format ("json" or "text") and level
// ("debug", "info", "warn", "error"; unknown values fall back to info).
// It returns the configured logger, which is also installed as default.
func Init(format, level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}

	l := slog.New(h)
	slog.SetDefault(l)
	return l
}
