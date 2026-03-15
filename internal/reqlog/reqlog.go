// Package reqlog provides per-request structured logging via context.
package reqlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

type contextKey struct{}

// FromContext retrieves the request-scoped logger from ctx.
// Returns slog.Default() if no logger is stored.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(contextKey{}).(*slog.Logger); ok {
		return l
	}

	return slog.Default()
}

// WithLogger stores a logger in the context.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, l)
}

// NewRequestID generates a 16-character hex string from 8 bytes of crypto/rand.
func NewRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns error on supported platforms
	return hex.EncodeToString(b[:])
}
