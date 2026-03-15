# Mirror Cache

Last verified: 2026-03-15

## Scope & Boundaries

Mirror-cache is a **caching fetch proxy**: go upstream, get bytes, put them in S3, serve from S3. It is a composable layer in a larger stack, not a complete system.

**The litmus test:** "Does this serve getting bytes into S3?" If yes, in scope.

### Out of Scope

- **Credential management** -- secrets live in Tokenizer (`github.com/superfly/tokenizer`), never in this process
- **Client identity/auth** -- could be layered via middleware, but the core doesn't participate
- **Content transformation** -- raw bytes only
- **Egress policy** -- egress proxy layer's concern (e.g., Smokescreen)

### Deliberate Policy Choices

- **Cache forever, validate with conditional requests.** Ignores upstream `Cache-Control`. This is a long-term mirror, not an HTTP-compliant cache
- **Redirect following is transparent.** Cache key is original URL, not final destination

## Commands

```bash
go build ./...
go test ./...
goimports -w .
```

## Architecture

HTTP caching proxy that stores upstream responses in S3 and serves cache hits via presigned URL redirects.

**Request flow:**
1. Client requests `/<domain>/<path>` (e.g., `/example.com/file.txt`)
2. `reqlog.Middleware` assigns a request ID and structured logger to the context
3. Server checks S3 for cached response headers (ETag, Last-Modified)
4. If cached: sends conditional request to upstream with `If-None-Match`/`If-Modified-Since`
5. On 304 Not Modified: redirects client to S3 presigned URL
6. On 200 OK: streams response to S3 cache, then redirects client

**Key components:**
- `server.go` - HTTP handler (`cacheMiddleware`): parses `/<domain>/<path>`, checks S3 cache, fetches upstream with singleflight dedup, redirects to presigned S3 URLs
- `cache.go` - `httpCache` interface: `Head`, `Put`, `GetPresignedURL`
- `s3_cache.go` - S3 implementation of `httpCache`
- `s3_metadata.go` - Serializes HTTP headers to/from S3 object metadata as JSON
- `fallback.go` - `FallbackPolicy`: controls when stale cached content is served on upstream errors
- `http_caching.go` - Injects conditional request headers from cached headers

**Internal packages:**
- `internal/reqlog` - Per-request structured logging: context helpers, request ID, HTTP middleware
- `internal/errorutil` - Error wrapping via `fmt.Errorf`
- `internal/etagging-server` - Test file server with auto-generated ETags
- `internal/singleflight` - Typed generic wrapper around `x/sync/singleflight`

## Configuration

Flags (with env var fallback):
- `--bucket` / `MIRROR_CACHE_BUCKET` (required) -- S3 bucket for cached responses
- `--prefix` / `MIRROR_CACHE_PREFIX` (default `cache`) -- S3 key prefix
- `--listen` (default `:8443`) -- listen address, ignored under socket activation
- `--egress-proxy` -- HTTP CONNECT proxy for upstream requests
- `--log-level` / `MIRROR_CACHE_LOG_LEVEL` (default `info`) -- log level: debug, info, warn, error

Stale-serving flags:
- `--stale-on-connection-error` (default true)
- `--stale-on-5xx` (default true)
- `--stale-on-any-error` (default false)

## Logging

Structured logging via `log/slog`. Format is auto-detected: JSON when stderr is not a TTY (production), text when it is (development). All request-scoped logging flows through `internal/reqlog` context helpers. Every HTTP response includes an `X-Request-ID` header for correlation.

## Infrastructure

Plain HTTP only. Supports systemd socket activation (`LISTEN_FDS`) and `sd_notify`. Uses AWS SDK default config (environment variables, ~/.aws/credentials, IAM role).

Terraform module: `terraform/module/mirror-cache/`
