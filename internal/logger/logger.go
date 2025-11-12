package logger

import (
	"log/slog"
	"os"
)

var (
	// Default logger instance
	Log *slog.Logger
)

func init() {
	// Initialize with a text handler for development
	// In production, use JSON handler for better log aggregation
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	Log = slog.New(handler)
}

// SetLevel changes the logging level
func SetLevel(level slog.Level) {
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	Log = slog.New(handler)
}

// SetJSON switches to JSON output (recommended for production)
func SetJSON() {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	Log = slog.New(handler)
}

// SetJSONWithLevel switches to JSON output with custom level
func SetJSONWithLevel(level slog.Level) {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	Log = slog.New(handler)
}
