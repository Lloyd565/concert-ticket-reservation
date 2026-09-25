package middleware

import (
	"context"
	"log/slog"
)

// LogHandler decorates a slog.Handler so that every record logged with a
// request-scoped context carries its correlation ID (ARCHITECTURE.md §7),
// without any call site having to remember to pass it.
type LogHandler struct{ slog.Handler }

// Handle adds the correlation ID, when there is one, and delegates.
func (h LogHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := CorrelationID(ctx); id != "" {
		r.AddAttrs(slog.String("correlation_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must rewrap, or the decoration is silently dropped
// the first time somebody calls log.With.
func (h LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return LogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup rewraps for the same reason as WithAttrs.
func (h LogHandler) WithGroup(name string) slog.Handler {
	return LogHandler{Handler: h.Handler.WithGroup(name)}
}
