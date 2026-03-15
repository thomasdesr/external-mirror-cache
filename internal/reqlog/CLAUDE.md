# reqlog

Last verified: 2026-03-15

## Purpose

Provides per-request structured logging so every log line within a request carries the same request ID without explicit threading.

## Contracts

- **Exposes**: `FromContext(ctx) → *slog.Logger`, `WithLogger(ctx, logger) → context.Context`, `NewRequestID() → string`, `Middleware(http.Handler) → http.Handler`
- **Guarantees**: `FromContext` never returns nil (falls back to `slog.Default()`). `Middleware` sets `X-Request-ID` response header and attaches a logger with `request_id` attribute to the context. Request IDs are 16 hex characters from crypto/rand.
- **Expects**: `slog.SetDefault()` called before `Middleware` runs (done in `main.go`'s `run()`)

## Dependencies

- **Uses**: `log/slog`, `crypto/rand` (stdlib only)
- **Used by**: `main.go` (middleware wrapping), `server.go`, `s3_cache.go` (via `FromContext`)
- **Boundary**: No external dependencies. Must not import any other internal package.

## Invariants

- `FromContext` on a context without a logger returns `slog.Default()`, never nil
- `Middleware` logs `request started` and `request completed` for every request
- `statusWriter` captures the first `WriteHeader` call; implicit 200 on `Write` without prior `WriteHeader`
