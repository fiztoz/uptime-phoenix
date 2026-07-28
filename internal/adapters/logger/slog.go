// Package logger provides adapters for the ports.Logger interface using log/slog.
package logger

import (
	"context"
	"log/slog"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// SlogLogger wraps log/slog to implement the ports.Logger interface.
type SlogLogger struct {
	logger *slog.Logger
}

// New creates a new SlogLogger with JSON output to stdout.
func New(level string) *SlogLogger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return &SlogLogger{logger: slog.New(handler)}
}

func (l *SlogLogger) Debug(msg string, args ...any) { l.logger.Debug(msg, args...) }
func (l *SlogLogger) Info(msg string, args ...any)  { l.logger.Info(msg, args...) }
func (l *SlogLogger) Warn(msg string, args ...any)  { l.logger.Warn(msg, args...) }
func (l *SlogLogger) Error(msg string, args ...any) { l.logger.Error(msg, args...) }

func (l *SlogLogger) With(args ...any) ports.Logger {
	return &SlogLogger{logger: l.logger.With(args...)}
}

func (l *SlogLogger) WithContext(ctx context.Context) ports.Logger {
	return &SlogLogger{logger: l.logger.With("request_id", ctx.Value("request_id"))}
}

// Ensure SlogLogger implements Logger.
var _ ports.Logger = (*SlogLogger)(nil)
