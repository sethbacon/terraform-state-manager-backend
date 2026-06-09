package telemetry

import (
	"log/slog"
	"os"
	"strings"
)

// SetupLogger installs a global slog default logger based on the supplied format
// and level strings from application configuration.
//
//	format: "json" → JSONHandler (production); anything else → TextHandler (local dev)
//	level:  "debug" | "info" | "warn" | "error" (case-insensitive); defaults to "info"
func SetupLogger(format, level string) {
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

	opts := &slog.HandlerOptions{
		Level:     lvl,
		AddSource: lvl == slog.LevelDebug,
	}

	var handler slog.Handler
	if strings.ToLower(format) == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	slog.SetDefault(slog.New(handler))
	slog.Info("logger initialised", "format", format, "level", lvl.String())
}
