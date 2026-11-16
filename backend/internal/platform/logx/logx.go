// Package logx создаёт структурированный JSON-логгер и переносит
// request id через context, чтобы его видели и HTTP, и Kafka.
package logx

import (
	"context"
	"log/slog"
	"os"
)

type ctxKey struct{}

// New создаёт JSON-логгер с полем service.
func New(service, level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: ParseLevel(level),
	})).With("service", service)
}

// ParseLevel переводит строку в уровень slog (как в orderflow).
func ParseLevel(value string) slog.Level {
	switch value {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// WithRequestID кладёт идентификатор запроса в контекст.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// RequestID достаёт идентификатор запроса из контекста.
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}
