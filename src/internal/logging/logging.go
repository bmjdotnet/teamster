package logging

import (
	"log/slog"
	"os"
)

func Init(component string) *slog.Logger {
	level := slog.LevelInfo
	if v := os.Getenv("TEAMSTER_LOG_LEVEL"); v != "" {
		switch v {
		case "debug":
			level = slog.LevelDebug
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	logger := slog.New(handler).With("component", component)
	slog.SetDefault(logger)
	return logger
}
