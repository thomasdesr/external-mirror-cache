# Mirror Cache

Last verified: 2026-08-17

## Scope & Boundaries

Mirror-cache is a **caching fetch proxy**: go upstream, get bytes, put them in S3, serve from S3. It is a composable layer in a larger stack, not a complete system. It composes via `http.RoundTripper` for outbound transport and plain HTTP for inbound.

### In Scope

**Transport: protocol adapters.** Self-contained units that know how to fetch from a specific upstream type. The default is plain HTTP GET. OCI is the first non-trivial adapter. Each adapter owns its protocol flow end-to-end: discovery, handshakes, retry logic, and deciding when credentials are needed. Adapters present a uniform `http.RoundTripper` interface to the core.

**Storage.** Cache keys, metadata serialization, presigned URLs, TTL/eviction. Cache keys are `CacheKey{URL, Variant}` pairs -- the core keys every request by its complete Accept set as the variant (URL-only when no Accept is sent). Non-empty variants produce distinct S3 objects and distinct singleflight groups.

**The litmus test:** "Does this serve getting bytes into S3?" If yes, in scope.

### Out of Scope

- **Credential management** -- secrets live in Tokenizer (`github.com/superfly/tokenizer`), never in this process. Protocol adapters decide *when* in their flow to invoke the shared Tokenizer client
- **Client identity/auth** -- could be layered via middleware, but the core doesn't participate
- **Content transformation** -- raw bytes only. Adapters may inspect content only when it's part of the fetch (e.g., parsing a manifest to discover what else to fetch)
- **Egress policy** -- egress proxy layer's concern (e.g., Smokescreen)
- **Upstream health monitoring** -- fetch on demand, serve stale on failure

### Protocol Adapter Guidelines

**Build an adapter when:**
- Upstream requires a multi-step handshake to fetch (OCI token negotiation)
- Credentials must be injected at a specific point in the protocol, not just on the top-level request
- Caching correctness requires protocol knowledge (mutable metadata vs immutable artifacts, snapshot consistency)
- The protocol has cache key requirements beyond the URL and Accept (the core already keys by both)

**Use the default (plain HTTP) when:** upstream is a simple HTTPS GET, optionally with credential header injection via the shared Tokenizer client.

**Adapters must not:** hold credentials, implement client auth, or implement their own storage (use the shared `httpCache` interface).

*Why adapters own credential routing (not a flat transport chain):* Different protocols need credentials at different points. OCI needs credentials on the token-fetch sub-request, not the registry request. A flat `tokenizerTransport -> ociAuthTransport -> http.Transport` chain can't express that -- and worse, an outer Tokenizer layer injecting `Authorization` headers would silently suppress OCI auth's challenge-response flow.

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
3. `keyFunc` builds a `CacheKey` from the target URL and request, and reports which header names the key's variant encodes (any request with an Accept header keys by its complete Accept set; Accept-less requests are URL-only, design in `docs/design-plans/2026-08-20-accept-variant-keys.md`)
4. Server checks S3 for the cached entry (stored headers plus S3 LastModified, object ETag, and size) using the `CacheKey`
5. With `--honor-freshness`: an entry that is fresh under RFC 9111 arithmetic -- Age-corrected current age under the declared lifetime AND resident time (now minus stored-at) under `--freshness-cap`, no-cache and Vary guarded (`internal/freshness`) -- redirects straight to the S3 presigned URL with zero upstream traffic. The cap bounds time since our last validation, not CDN residency reported in `Age`
6. Otherwise, if cached: sends conditional request to upstream with `If-None-Match`/`If-Modified-Since` (and Accept header if present)
7. On 304 Not Modified -- or a 200 whose strong ETag matches the cached one (some upstreams ignore conditional headers) -- redirects client to S3 presigned URL without re-uploading. With the flag on, if the §4.3.4-merged headers would make the entry fresh at the instant of the touch -- resident time zero, though the response's Age still counts against the declared lifetime -- (and the response's ETag doesn't contradict the stored one), the entry is *touched*: a conditional S3 metadata self-copy that re-arms its freshness window without a body re-upload
8. On any other 200 OK: streams response to S3 cache, then redirects client. A cache-write failure always errors -- S3 trouble is mirror infrastructure and must surface, never be masked by stale-serving

Upstream redirects are followed before caching -- the cache key is the original requested URL (plus variant if applicable), not the final redirect destination.

**Key components:**
- `server.go` - Main HTTP handler (`cacheMiddleware`): parses `/<domain>/<path>` requests, builds a `CacheKey` via pluggable `keyFunc`, checks S3 cache, fetches upstream with singleflight dedup (keyed on `CacheKey.String()`), forwards Accept header to upstream, redirects clients to presigned S3 URLs. `acceptVariantKeyFunc` keys every request by its complete Accept set (URL-only when no Accept is sent)
- `cache.go` - `CacheKey` type (URL + Variant), the `cachedEntry` record (headers, stored-at, object ETag, size), and the `httpCache` interface: `Head`, `Put`, `GetPresignedURL`, `Touch` (all take `CacheKey`)
- `s3_cache.go` - S3 implementation of `httpCache`: stores responses, serves presigned URLs, and implements `Touch` as a CopyObject metadata self-copy -- conditional on the Head-time object ETag so a touch racing a concurrent `Put` loses with a 412, re-supplying ContentType (REPLACE resets system metadata), skipping objects over the 5GB single-part copy limit. S3 key grammar is `<prefix>/<escaped host><escaped path>[?<escaped query>][??<escaped variant>]`, stdlib-escaped so no component contains a raw `?` -- every `?` is a joiner, the grammar is injective, and the escaping is the identity on real traffic so keys read like the URLs they cache
- `s3_metadata.go` - Serializes HTTP headers to/from S3 object metadata as JSON. Writes only an allowlisted subset (validators, entity headers, freshness fields) -- S3 caps user metadata at 2KB and full upstream header sets exceed it; reads stay permissive for the pre-allowlist corpus
- `fallback.go` - `FallbackPolicy`: controls when stale cached content is served on upstream errors. Upstream only by design: cache-write (S3) failures never consult it
- `http_caching.go` - Injects conditional request headers from cached headers
- `oci_auth.go` - OCI Bearer token auth transport. Intercepts 401 challenges from OCI registries (e.g., Docker Hub), fetches anonymous tokens, caches them with TTL, and retries. Uses singleflight to deduplicate concurrent token fetches. Proactive path reuses cached challenges to avoid discovery round-trips. Only activates for `/v2/` OCI paths; non-OCI requests and requests with existing Authorization headers pass through. Transport chain: `http.Client` -> `ociAuthTransport` -> `http.Transport`

**Internal packages:**
- `internal/freshness` - The RFC 9111 freshness gate (pure function of stored headers, stored-at, now, cap, variant-encoded key headers) and the §4.3.4 header merge. Every parse ambiguity fails open to revalidation; design in `docs/design-plans/2026-08-04-rfc9111-freshness.md`
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

Upstream timeout flags:
- `--response-header-timeout` / `MIRROR_CACHE_RESPONSE_HEADER_TIMEOUT` (default `5s`) -- max wait for upstream response headers; conditional fetches fall back to stale on timeout, cache-miss returns 502; `0` disables
- `--client-timeout` / `MIRROR_CACHE_CLIENT_TIMEOUT` (default `120s`) -- overall `http.Client.Timeout`, bounds total upstream request time including body streaming; `0` disables

Freshness flags:
- `--honor-freshness` / `MIRROR_CACHE_HONOR_FRESHNESS` (default `false`) -- serve fresh cache hits without revalidating; gates both the fresh fast path and the revalidation touch
- `--freshness-cap` / `MIRROR_CACHE_FRESHNESS_CAP` (default `168h`) -- max resident time before a fresh-eligible entry revalidates; `0` means nothing is ever served fresh (not "disable the bound")

## Logging

Structured logging via `log/slog`. Format is auto-detected: JSON when stderr is not a TTY (production), text when it is (development). All request-scoped logging flows through `internal/reqlog` context helpers. Every HTTP response includes an `X-Request-ID` header for correlation.

## Infrastructure

Plain HTTP only. Supports systemd socket activation (`LISTEN_FDS`) and `sd_notify`. Uses AWS SDK default config (environment variables, ~/.aws/credentials, IAM role).

Terraform module: `terraform/module/mirror-cache/`
