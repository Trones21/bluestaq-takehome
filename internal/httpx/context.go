package httpx

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyLogger
	ctxKeyUserID
)

// WithRequestID attaches a request ID to the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

func RequestID(r *http.Request) string {
	id, _ := r.Context().Value(ctxKeyRequestID).(string)
	return id
}

func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, l)
}

// Logger returns the request-scoped logger, which already carries the request
// ID and user ID. Falls back to the default logger so handlers never have to
// nil-check.
func Logger(r *http.Request) *slog.Logger {
	if l, ok := r.Context().Value(ctxKeyLogger).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

func WithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKeyUserID, id)
}

// UserID returns the authenticated caller. The second result is false on
// unauthenticated routes; every route behind the auth middleware can rely on
// it being true.
func UserID(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(ctxKeyUserID).(uuid.UUID)
	return id, ok
}

// MustUserID is for handlers mounted behind the auth middleware, where an
// absent user is a routing bug rather than a client error.
func MustUserID(ctx context.Context) uuid.UUID {
	id, ok := UserID(ctx)
	if !ok {
		panic("httpx: no authenticated user in context; handler mounted outside auth middleware")
	}
	return id
}
