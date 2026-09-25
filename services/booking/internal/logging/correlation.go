// Package logging carries the request correlation ID from gRPC metadata onto
// every log line the request produces (ARCHITECTURE.md §7).
//
// The gateway is what mints the ID; this end only reads it. A request that
// arrives without one is logged without one rather than being given a fresh
// ID here - a second ID for the same request is worse than none, because it
// looks like a separate request when the logs are joined.
package logging

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// MetadataKey is the gRPC metadata key the gateway propagates the ID under.
// gRPC lowercases metadata keys, so this constant must stay lowercase.
const MetadataKey = "x-correlation-id"

type ctxKey struct{}

// WithCorrelationID attaches an ID to a context.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// CorrelationID returns the ID attached to ctx, or "".
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// Handler decorates a slog.Handler so that every record logged with a
// request-scoped context carries its correlation ID, without any call site
// having to remember to pass it.
type Handler struct{ slog.Handler }

// Handle adds the correlation ID, when there is one, and delegates.
func (h Handler) Handle(ctx context.Context, r slog.Record) error {
	if id := CorrelationID(ctx); id != "" {
		r.AddAttrs(slog.String("correlation_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must rewrap, or the decoration is silently dropped
// the first time somebody calls log.With.
func (h Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return Handler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup rewraps for the same reason as WithAttrs.
func (h Handler) WithGroup(name string) slog.Handler {
	return Handler{Handler: h.Handler.WithGroup(name)}
}

// UnaryServerInterceptor lifts the correlation ID out of incoming metadata and
// into the request context.
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if values := md.Get(MetadataKey); len(values) > 0 && values[0] != "" {
				ctx = WithCorrelationID(ctx, values[0])
			}
		}
		return handler(ctx, req)
	}
}
