# Structured Logging Design

## Summary

Mirror-cache currently logs with the stdlib `log` package, which produces unstructured text with no correlation between log lines from the same request. This design migrates the entire codebase to `log/slog` -- Go's standard structured logging library -- and threads a unique request ID through every log line emitted during a request's lifetime. The result is that a single `grep` or log query on a request ID surfaces every relevant event: cache lookup, upstream fetch, OCI token negotiation, S3 read/write, and final response.

The implementation is additive at the interface level. A new `internal/reqlog` package provides a context key, a logger accessor, and an HTTP middleware that generates the request ID and attaches a pre-tagged logger to each request's context. Existing functions already accept `context.Context` as their first parameter, so the call sites in `server.go`, `oci_auth.go`, and `s3_cache.go` change from bare `log.Printf` calls to `reqlog.FromContext(ctx).Info(...)` calls with structured key-value attributes. Handler selection (JSON for non-TTY, text for TTY) and level control (`--log-level` flag / `MIRROR_CACHE_LOG_LEVEL` env var) are configured once at startup in `main.go` and apply globally via `slog.SetDefault`.

## Definition of Done

1. **All logging uses `log/slog`** -- every `log.Printf`/`log.Println` replaced with leveled, structured slog calls. Zero remaining imports of `"log"` in non-test code.
2. **Request ID threaded everywhere** -- every inbound request gets a unique ID, stored in context, present on every log line for that request (including through OCI auth transport and S3 operations). Singleflight work logs under the leader's request ID; followers log their own IDs on their own entry/exit lines.
3. **Log level controllable** -- `--log-level` flag / `MIRROR_CACHE_LOG_LEVEL` env var, default INFO.
4. **JSON output by default, text if stderr is a TTY** -- no new dependencies (stdlib TTY detection via `os.Stderr.Stat()`).
5. **Currently-silent code paths get logging** -- S3 operations, presign, and other gaps filled with appropriate-level log calls.

## Acceptance Criteria

### structured-logging.AC1: All logging uses `log/slog`
- **structured-logging.AC1.1 Success:** Every `log.Printf`/`log.Println` in `main.go`, `server.go`, `oci_auth.go`, `s3_cache.go` replaced with `slog` calls at appropriate levels
- **structured-logging.AC1.2 Success:** `grep -rn '"log"' *.go` returns no matches in non-test production code (excluding `internal/etagging-server`)

### structured-logging.AC2: Request ID threaded everywhere
- **structured-logging.AC2.1 Success:** Each inbound request generates a unique 16-char hex request ID via `crypto/rand`
- **structured-logging.AC2.2 Success:** Every log line emitted during request processing includes `request_id` attribute
- **structured-logging.AC2.3 Success:** `X-Request-ID` response header set on every response
- **structured-logging.AC2.4 Success:** `reqlog.FromContext(ctx)` returns `slog.Default()` when no logger in context (graceful fallback)
- **structured-logging.AC2.5 Edge:** Singleflight work logs under the leader's request ID; follower requests log their own IDs on middleware entry/exit

### structured-logging.AC3: Log level controllable
- **structured-logging.AC3.1 Success:** `--log-level=debug` shows Debug-level messages
- **structured-logging.AC3.2 Success:** `MIRROR_CACHE_LOG_LEVEL=warn` suppresses Info messages
- **structured-logging.AC3.3 Failure:** Invalid log level (e.g., `--log-level=banana`) returns error at startup

### structured-logging.AC4: JSON/text handler selection
- **structured-logging.AC4.1 Success:** Non-TTY stderr produces JSON-formatted log lines
- **structured-logging.AC4.2 Success:** TTY stderr produces text-formatted log lines

### structured-logging.AC5: Currently-silent code paths get logging
- **structured-logging.AC5.1 Success:** `server.go` logs: cache head result, upstream fetch start, upstream response outcome, fallback decisions -- all with structured `target` attribute
- **structured-logging.AC5.2 Success:** `oci_auth.go` logs: auth bypass, non-OCI passthrough, proactive token cache hit, token fetch success -- all with `host`/`repo` attributes
- **structured-logging.AC5.3 Success:** `s3_cache.go` logs: `Head`/`Put`/`GetPresignedURL` operations with `bucket`/`key` attributes

## Glossary

- **`log/slog`**: Go standard library package (added in Go 1.21) for structured, leveled logging. Replaces the older `log` package. Log calls produce key-value attribute pairs rather than free-form strings, and the output format (JSON or text) is determined by the handler, not the call site.
- **structured logging**: A logging style where each log entry is a machine-parseable record with typed fields (e.g., `{"level":"info","request_id":"a3f1...","status":200}`) rather than a formatted string. Enables filtering and aggregation in log query systems.
- **request ID**: A unique identifier generated per inbound HTTP request, attached to every log line emitted during that request. Allows all log entries for a single request to be correlated.
- **context-threaded logger**: A `*slog.Logger` stored in the request's `context.Context` and retrieved at each call site via `reqlog.FromContext(ctx)`. Avoids passing the logger as an explicit parameter through every function.
- **`http.RoundTripper`**: A Go interface (`RoundTrip(*http.Request) (*http.Response, error)`) representing a single HTTP transaction. Used here as the extension point for transport-level concerns like OCI auth.
- **singleflight**: A deduplication pattern where concurrent identical requests share a single in-flight upstream fetch. Only one goroutine does the actual work; others wait and receive the same result.
- **TTY detection**: Checking whether a file descriptor is connected to a terminal (character device) using `os.File.Stat()` and the `ModeCharDevice` bit. Used here to choose between human-readable text logs (TTY) and machine-readable JSON logs (non-TTY).
- **OCI (Open Container Initiative)**: The standard governing container image formats and distribution. OCI registries use a specific HTTP API under `/v2/` with a Bearer token challenge-response auth flow.
- **Bearer token challenge-response**: The OCI registry auth pattern: the client makes a request, receives a `401` with a `WWW-Authenticate: Bearer realm=...` header, fetches an anonymous token from that endpoint, then retries with `Authorization: Bearer <token>`.
- **proactive token reuse**: An optimization in `ociAuthTransport` that checks the token cache before making the initial registry request, avoiding the 401 round-trip when a valid token is already known.
- **presigned URL**: A time-limited, pre-authenticated URL granting direct access to a private S3 object without requiring the client to have AWS credentials.
- **`reqlog` middleware**: The `internal/reqlog.Middleware` HTTP handler wrapper that generates a request ID, attaches a tagged logger to the context, sets the `X-Request-ID` response header, and logs request start/end events.

## Architecture

Replace stdlib `log` with `log/slog` throughout the codebase. Thread a per-request logger carrying a unique request ID through context, so every log line for a request is correlated.

### Logger Setup (`main.go`)

`run()` configures the default slog logger before any other initialization:

- **Handler selection:** `os.Stderr.Stat()` checks `ModeCharDevice` to detect TTY. TTY gets `slog.NewTextHandler`, non-TTY gets `slog.NewJSONHandler`. Both write to `os.Stderr`.
- **Level control:** New flag `--log-level` with env fallback `MIRROR_CACHE_LOG_LEVEL`, default `"info"`. Parsed via `slog.Level.UnmarshalText` (handles `debug`, `info`, `warn`, `error` case-insensitively). Applied via `slog.HandlerOptions.Level`.
- **Startup logging:** After setup, log configuration at Info (bucket, prefix, listen address, egress proxy, log level, log format).

### Context-Threaded Logger (`internal/reqlog/`)

New package `internal/reqlog` with:

- `FromContext(ctx context.Context) *slog.Logger` -- retrieves request-scoped logger from context, falls back to `slog.Default()`.
- `WithLogger(ctx context.Context, l *slog.Logger) context.Context` -- stores logger in context via `context.WithValue`.
- `NewRequestID() string` -- generates 16-character hex string from 8 bytes of `crypto/rand`.
- `Middleware(next http.Handler) http.Handler` -- HTTP middleware that:
  1. Generates request ID
  2. Creates child logger: `slog.Default().With("request_id", id)`
  3. Stores logger in context via `WithLogger`
  4. Sets `X-Request-ID` response header
  5. Logs request start at Info: `method`, `path`, `remote_addr`
  6. Wraps `http.ResponseWriter` to capture status code
  7. Logs request end at Info: `method`, `path`, `status`, `duration`

Call sites assign once at function top: `logger := reqlog.FromContext(ctx)`, then use `logger.Info(...)` etc. throughout.

### Data Flow

```
Client request
  -> reqlog.Middleware (generates request_id, stores logger in ctx)
    -> cacheMiddleware.ServeHTTP (uses reqlog.FromContext(ctx))
      -> s3HTTPCache.Head(ctx, ...) (uses reqlog.FromContext(ctx))
      -> http.Client.Do(req)
        -> ociAuthTransport.RoundTrip(req) (uses reqlog.FromContext(req.Context()))
          -> base transport
      -> s3HTTPCache.Put(ctx, ...) (uses reqlog.FromContext(ctx))
      -> s3HTTPCache.GetPresignedURL(ctx, ...) (uses reqlog.FromContext(ctx))
```

The singleflight in `cacheMiddleware` uses `context.WithoutCancel(r.Context())`, which preserves context values (including the stored logger). The leader's request ID appears on all singleflight-internal logs. Followers log their own request IDs on their own `Middleware` entry/exit lines.

### Log Level Assignments

**Info:** Request lifecycle (start/end), upstream response outcomes (304, 200), challenge discovery, startup config, listener ready, shutdown.

**Debug:** Cache head miss, upstream fetch start, presign generated, cache put completed, S3 operations, OCI auth bypass/passthrough, proactive token reuse, redirect following.

**Warn:** Stale fallback served, token fetch failed (non-fatal), stale token re-discovery, cache head errors, same challenge already failed.

**Error:** Fetch-and-cache failure (returned to client), shutdown error.

## Existing Patterns

Investigation found no existing structured logging or context-value threading. The codebase uses:

- `internal/` packages for encapsulated utilities (`internal/errorutil`, `internal/singleflight`, `internal/etagging-server`). The new `internal/reqlog` package follows this convention.
- `http.Handler` as the main entry point (`cacheMiddleware.ServeHTTP`). The middleware wraps this, wired in `main.go`.
- `http.RoundTripper` for transport-level concerns (`ociAuthTransport`). The transport accesses the request-scoped logger via `req.Context()`, consistent with how it already accesses context for cancellation.
- All functions that do I/O already accept `context.Context` as first parameter (`Head`, `Put`, `GetPresignedURL`, `fetchAndCache`, `fetchToken`). No signature changes needed.

No divergence from existing patterns. This design adds a new internal package and a middleware layer; all existing interfaces and signatures remain unchanged.

## Implementation Phases

<!-- START_PHASE_1 -->
### Phase 1: `internal/reqlog` Package

**Goal:** Create the request logging package with context helpers, request ID generation, and HTTP middleware.

**Components:**
- `internal/reqlog/reqlog.go` -- `FromContext`, `WithLogger`, `NewRequestID`, context key type
- `internal/reqlog/middleware.go` -- `Middleware` function, response writer wrapper (captures status code)
- Tests for request ID uniqueness, context round-trip, middleware behavior (status capture, header setting, start/end logging)

**Dependencies:** None (first phase).

**Done when:** `go build ./internal/reqlog/...` succeeds, tests pass verifying: request IDs are unique, `FromContext` returns stored logger, `FromContext` falls back to `slog.Default()` when no logger in context, middleware sets `X-Request-ID` header, middleware logs request start and end with correct attributes.

**Covers:** structured-logging.AC2.1, structured-logging.AC2.2, structured-logging.AC2.3, structured-logging.AC2.4, structured-logging.AC4.1
<!-- END_PHASE_1 -->

<!-- START_PHASE_2 -->
### Phase 2: slog Setup and Flag in `main.go`

**Goal:** Configure slog as the default logger with handler selection and level control. Wire `reqlog.Middleware` into the server.

**Components:**
- `main.go` -- slog handler setup (TTY detection, level parsing), new `--log-level` flag with `MIRROR_CACHE_LOG_LEVEL` env fallback, `reqlog.Middleware` wrapping `cacheMiddleware`, replace all `log.*` calls with `slog.*`
- Remove `"log"` import from `main.go`

**Dependencies:** Phase 1 (`internal/reqlog` exists).

**Done when:** `go build ./...` succeeds with no `"log"` import in `main.go`. Startup logs config at Info. `--log-level=debug` shows debug messages. JSON output when stderr is not a TTY, text when it is. Middleware is wired and requests get request IDs.

**Covers:** structured-logging.AC1.1, structured-logging.AC3.1, structured-logging.AC3.2, structured-logging.AC3.3, structured-logging.AC4.1, structured-logging.AC4.2
<!-- END_PHASE_2 -->

<!-- START_PHASE_3 -->
### Phase 3: Migrate `server.go` Logging

**Goal:** Replace all `log.*` calls in `server.go` with structured `reqlog.FromContext(ctx).*` calls at appropriate levels.

**Components:**
- `server.go` -- replace all `log.Println`/`log.Printf` calls with `logger := reqlog.FromContext(ctx)` pattern. Add structured attributes (`target`, `status`, `error`, `has_cached`, `content_type`). Remove request lifecycle logging (middleware handles it). Remove `"log"` import.

**Dependencies:** Phase 2 (slog configured, middleware wired).

**Done when:** Zero `"log"` import in `server.go`. All log calls use `reqlog.FromContext(ctx)` with structured attributes. Existing integration tests pass. Log output includes request_id on all server log lines.

**Covers:** structured-logging.AC1.1, structured-logging.AC2.2, structured-logging.AC5.1
<!-- END_PHASE_3 -->

<!-- START_PHASE_4 -->
### Phase 4: Migrate `oci_auth.go` Logging

**Goal:** Replace all `log.*` calls in `oci_auth.go` with structured slog calls via context, and add logging to currently-silent paths.

**Components:**
- `oci_auth.go` -- replace `log.Printf` calls with `reqlog.FromContext(req.Context())` pattern. Add structured attributes (`host`, `repo`, `realm`, `error`). Add logging to silent paths: auth bypass (Debug), non-OCI passthrough (Debug), proactive token cache hit (Debug), token fetch success (Debug). Remove `"log"` import.

**Dependencies:** Phase 2 (slog configured).

**Done when:** Zero `"log"` import in `oci_auth.go`. All log calls use `reqlog.FromContext(ctx)`. Silent paths now log at Debug. Existing OCI auth tests pass. Request ID appears on OCI auth log lines.

**Covers:** structured-logging.AC1.1, structured-logging.AC2.2, structured-logging.AC5.2
<!-- END_PHASE_4 -->

<!-- START_PHASE_5 -->
### Phase 5: Add Logging to `s3_cache.go`

**Goal:** Add structured debug logging to all S3 operations.

**Components:**
- `s3_cache.go` -- add `reqlog.FromContext(ctx)` logging to `Head` (call, miss, error), `Put` (start, complete), `GetPresignedURL` (call). Structured attributes: `bucket`, `key`.

**Dependencies:** Phase 2 (slog configured).

**Done when:** S3 operations log at Debug with `bucket` and `key` attributes. Request ID appears on S3 log lines. Existing tests pass. `go vet ./...` clean.

**Covers:** structured-logging.AC1.1, structured-logging.AC5.3
<!-- END_PHASE_5 -->

<!-- START_PHASE_6 -->
### Phase 6: Verification and Cleanup

**Goal:** Verify zero `"log"` imports remain in non-test code, all tests pass, and log output is correct end-to-end.

**Components:**
- Verify: `grep -r '"log"' *.go` returns no matches (excluding test files and `internal/etagging-server`)
- Run full test suite: `go test ./...`
- Run `go vet ./...` and `goimports -w .`
- Update CLAUDE.md architecture description to reflect `internal/reqlog` package

**Dependencies:** Phases 3, 4, 5 (all migrations complete).

**Done when:** No `"log"` imports in non-test production code (excluding `internal/etagging-server`). Full test suite passes. `go vet` clean. CLAUDE.md updated.

**Covers:** structured-logging.AC1.1, structured-logging.AC1.2
<!-- END_PHASE_6 -->

## Additional Considerations

**Health check logging:** The `/health` endpoint now gets logged by `reqlog.Middleware`. This is useful for visibility but could be noisy under frequent ALB health checks. If it becomes a problem, a future change could log health checks at Debug level. Not addressing now — wait for evidence.

**`internal/etagging-server`:** This is a dev tool (`go run ./internal/etagging-server`), not production code. It keeps its `"log"` import — no value in migrating a dev utility.

**Singleflight request ID semantics:** The leader's request ID appears on shared fetch work. This is correct and intentional — the fetch is one logical operation regardless of how many requests wait for it. Each waiting request logs its own request ID on its entry/exit lines via the middleware.
