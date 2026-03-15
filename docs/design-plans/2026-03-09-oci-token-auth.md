# OCI Token Auth Design

## Summary

The mirror cache already proxies HTTP requests from clients and caches responses in S3, but it currently has no mechanism to authenticate with OCI-compliant container registries (such as Docker Hub). These registries protect their APIs with a token-based challenge-response protocol: an unauthenticated request returns a 401 with a `WWW-Authenticate` header describing where and how to obtain an anonymous bearer token. Without handling this, the proxy surfaces 401 errors to callers instead of the cached content they expect.

This design adds an `ociAuthTransport`, a thin wrapper around the existing HTTP transport that intercepts OCI auth challenges before the application layer sees them. The transport resolves challenges invisibly: it detects 401 responses bearing an OCI Bearer challenge, fetches an anonymous token from the registry's auth endpoint, caches it in memory with TTL awareness, and retries the original request. On subsequent requests to the same registry scope, the cached token is attached proactively, reducing auth to zero extra round trips. Token fetches for the same scope are deduplicated via singleflight so concurrent callers don't stampede the auth endpoint. The rest of the proxy -- `fetchAndCache`, S3 caching, conditional revalidation -- is entirely unchanged.

## Definition of Done

The mirror cache transparently handles OCI registry Bearer token challenges. When upstream returns 401 + WWW-Authenticate, the proxy fetches an anonymous token, retries with it, and caches the result. Clients never see auth challenges for successful public OCI flows. (When the transport cannot resolve a challenge — token endpoint down, malformed response, or invalid token — the upstream 401 surfaces to the caller.)

**Success criteria:**
- `oci_pull` can pull public images from Docker Hub (and any OCI registry using Bearer token auth) through the mirror cache without errors
- Both initial fetches and cache revalidation requests work with token auth

**Out of scope:**
- Header passthrough on error responses
- Authenticated/private repo pulls
- Shared token state across instances

## Acceptance Criteria

### oci-token-auth.AC1: Proxy resolves OCI Bearer challenges transparently
- **oci-token-auth.AC1.1 Success:** Transport detects 401 + WWW-Authenticate Bearer with realm, service, scope
- **oci-token-auth.AC1.2 Success:** Challenge parsed correctly into (realm, service, scope) components
- **oci-token-auth.AC1.3 Success:** Anonymous token fetched from realm endpoint with service and scope params
- **oci-token-auth.AC1.4 Success:** First request to auth-required registry succeeds (discovery path: bare request, 401, token fetch, resend with token, 200)
- **oci-token-auth.AC1.5 Success:** Subsequent request to same scope uses cached token (proactive path: single round trip)
- **oci-token-auth.AC1.6 Success:** Near-expiry token refreshed before request (refresh path)
- **oci-token-auth.AC1.7 Success:** Initial fetch through full proxy caches artifact, client gets 303 redirect
- **oci-token-auth.AC1.8 Success:** Cache revalidation with If-None-Match works through auth-required registry (304 path)

### oci-token-auth.AC2: Token cache manages lifecycle
- **oci-token-auth.AC2.1 Success:** Tokens stored and retrieved by (realm, service, scope) key
- **oci-token-auth.AC2.2 Success:** Expired tokens not returned from cache
- **oci-token-auth.AC2.3 Success:** Concurrent cache access is safe
- **oci-token-auth.AC2.4 Success:** Concurrent token fetches for same scope deduplicated via singleflight

### oci-token-auth.AC3: Non-OCI and unresolvable auth requests handled correctly
- **oci-token-auth.AC3.1 Passthrough:** 401 without WWW-Authenticate header passes through unchanged
- **oci-token-auth.AC3.2 Passthrough:** 401 with Basic (not Bearer) challenge passes through unchanged
- **oci-token-auth.AC3.3 Failure:** Token fetch failure (network error, non-200 from realm) returns the upstream 401 response to the caller
- **oci-token-auth.AC3.4 Failure:** Malformed token JSON from realm returns the upstream 401 response to the caller
- **oci-token-auth.AC3.5 Failure:** Resend with token still gets 401 — surfaces the second 401 to the caller, no retry loop
- **oci-token-auth.AC3.6 Passthrough:** Request with existing `Authorization` header bypasses OCI token logic entirely

### oci-token-auth.AC4: End-to-end integration
- **oci-token-auth.AC4.1 Success:** oci_pull-style bare request through proxy gets 303 (never sees 401)
- **oci-token-auth.AC4.2 Success:** Multiple sequential requests: first caches, second revalidates, both return 303

## Glossary

- **OCI (Open Container Initiative)**: Standards body defining the image format and distribution API used by container registries. Docker Hub and most other registries implement the OCI Distribution Specification.
- **Bearer token / Bearer challenge**: HTTP authentication scheme where the server returns `WWW-Authenticate: Bearer` on 401, directing the client to obtain a short-lived token and re-present it as `Authorization: Bearer <token>`.
- **WWW-Authenticate**: HTTP response header returned with 401 responses describing how a client should authenticate. OCI registries use the `Bearer` scheme with `realm`, `service`, and `scope` parameters.
- **realm, service, scope**: The three parameters in an OCI Bearer challenge. `realm` is the token endpoint URL; `service` identifies the registry; `scope` describes the specific resource and permission (e.g., `repository:library/ubuntu:pull`).
- **http.RoundTripper**: Go interface with a single `RoundTrip` method representing one HTTP transaction. Wrapping a RoundTripper is the standard Go pattern for injecting middleware (auth, retries, logging) into an HTTP client.
- **singleflight**: Go concurrency pattern that deduplicates in-flight calls with the same key. Multiple goroutines requesting the same token share one fetch; only one HTTP call goes out.
- **TTL (Time To Live)**: Duration a cached value is considered valid, derived from the `expires_in` field in the token response. Implementation subtracts a 30-second buffer to avoid using tokens at edge of expiry.
- **fetchAndCache**: Core proxy handler in `server.go` that checks S3 for a cached response, sends conditional requests upstream, and stores new responses.
- **presigned URL / 303 redirect**: When the proxy has a cached artifact, it redirects the client (HTTP 303) to a time-limited S3 URL rather than proxying the bytes itself.
- **conditional request / cache revalidation**: HTTP request with `If-None-Match` or `If-Modified-Since` headers, asking the server to return 304 Not Modified if the cached copy is current.
- **oci_pull**: Bazel rule from `rules_oci` that fetches OCI images from container registries. The concrete client motivating this design -- it relies on Bazel's HTTP downloader to handle registry auth challenges.

## Architecture

New `ociAuthTransport` implementing `http.RoundTripper`, wrapping the existing `http.Transport`. Sits between `fetchAndCache` and the network at the same layer where Go's `http.Client` resolves redirects.

```
main.go wiring:

  http.Transport (timeouts, egress proxy)
    wrapped by ociAuthTransport (resolves OCI Bearer challenges)
      used by http.Client (resolves redirects)
        used by cacheMiddleware.fetchAndCache (only sees 200/304/error)
```

`fetchAndCache` is unchanged. It never sees 401 responses from auth-required registries.

**Client Authorization precedence:** If the inbound request already carries an `Authorization` header, the transport passes it through unchanged and skips OCI token logic entirely. This proxy handles anonymous public pulls only; authenticated/private repo pulls are out of scope.

**Resend constraints:** The transport only resends GET and HEAD requests after obtaining a token. OCI distribution API pulls are exclusively GET requests, so this covers all expected traffic. Non-idempotent methods or requests with non-replayable bodies are never resent — the 401 passes through.

### Transport flow

The transport manages token lifecycle proactively, similar to `x/oauth2`'s token-refreshing transport:

1. **Proactive path (steady state):** Request to a known auth-required host with a valid cached token for the scope. Transport looks up the scope using the URL-to-scope mapping (populated during discovery), finds the cached token, attaches `Authorization: Bearer <token>`, and sends. Single round trip.

2. **Discovery path (first contact per scope):** Request to unknown host or new scope. Transport sends bare request. If response is 401 with OCI Bearer challenge (see detection below), transport parses challenge, fetches token, caches the token keyed by `(realm, service, scope)`, records the URL path → `(realm, service, scope)` mapping for future proactive use, attaches the token, and resends. Two round trips, happens once per (host, scope) per token TTL.

3. **Refresh path:** Cached token is near expiry (less than 30 seconds remaining). Transport fetches a fresh token (via singleflight) before attaching. Single round trip from the caller's perspective.

**Scope derivation for proactive path:** The transport maintains a `challenges` map from `(host, repository path)` to the learned `(realm, service, scope)` from previous 401 responses. OCI repository names can contain slashes (e.g., `my-org/my-project/my-image`), so the repository path is extracted by stripping the `/v2/` prefix and the trailing API action segment (`/manifests/...`, `/blobs/...`, `/tags/...`). For example, `/v2/my-org/my-image/manifests/latest` yields repository path `my-org/my-image`. When a new request arrives, the transport extracts the repository path the same way and looks up the exact match in the challenges map — no prefix matching needed, since scope maps 1:1 to repository name. If a match is found and a valid token exists for that scope, it's attached proactively. New repositories on a known host still require one discovery round trip to learn their scope.

### OCI challenge detection

A 401 response is treated as an OCI auth challenge only when all of these are true:
- HTTP status is 401
- `WWW-Authenticate` header is present with `Bearer` scheme (case-insensitive scheme matching per RFC 7235)
- Parsed challenge contains `realm`, `service`, and `scope` parameters (case-insensitive parameter keys, values may be quoted per RFC 7235)

Non-OCI 401s (missing challenge, Basic auth, incomplete parameters) pass through untouched.

**Parsing robustness:** The challenge parser handles multiple `WWW-Authenticate` headers (picks the first Bearer challenge), case-insensitive scheme and parameter key matching, and quoted string values with escaped characters. If `scope` is absent, the 401 is not treated as an OCI challenge — it passes through.

### Token fetch

Anonymous GET to the realm endpoint:
```
GET {realm}?service={service}&scope={scope}
```

Returns JSON:
```json
{"token": "...", "expires_in": 3600}
```

**Token response compatibility:** The token fetch accepts both `token` and `access_token` fields (Docker Hub uses `token`, some registries use `access_token` per the Docker Registry Token Authentication spec). If `expires_in` is missing or zero, the transport defaults to 60 seconds — short enough to be safe, long enough to avoid constant re-fetches.

No credentials needed for public repository pulls. The fetch uses the base transport directly (not `ociAuthTransport`) to avoid recursion — token endpoints don't issue Bearer challenges.

### Components

All in the module root alongside `server.go`, `http_client_transport.go`, etc.:

- **`ociAuthTransport`** — `http.RoundTripper` wrapper. Fields: `base http.RoundTripper`, `tokens *tokenCache`, `challenges` map of `(host, repository)` to learned `(realm, service, scope)` (guarded by `sync.RWMutex` — locks protect map operations only, never held during network calls), `flights` typed singleflight group for token fetches.
- **`isOCIAuthChallenge(resp *http.Response) bool`** — checks response for OCI Bearer challenge signature.
- **`parseOCIAuthChallenge(resp *http.Response) ociAuthChallenge`** — extracts realm, service, scope from `WWW-Authenticate` header.
- **`ociAuthChallenge`** — struct with `Realm`, `Service`, `Scope` fields.
- **`tokenCache`** — in-memory, concurrency-safe. Key: `(realm, service, scope)` string. Value: token + expiry. Lazy eviction on read. `sync.Mutex` with map (need atomic check-and-use of expiry). Subtracts 30-second buffer from `expires_in` to avoid edge-of-expiry failures.

### Data flow for cache revalidation

After initial pull, the proxy has the artifact in S3. On revalidation:

1. `fetchAndCache` builds conditional request (`If-None-Match`)
2. `ociAuthTransport` recognizes the host, attaches cached token
3. Upstream returns 304 (content unchanged, token valid)
4. `fetchAndCache` redirects client to S3 presigned URL

Single round trip. Client never sees auth.

## Existing Patterns

The proxy already resolves upstream complexity at the transport/client layer. HTTP redirects are followed by `http.Client` (configured in `main.go:94-102` with `CheckRedirect` logging). `fetchAndCache` only sees the final response. OCI auth challenge resolution follows this same pattern — resolve before the application layer sees it.

The singleflight pattern used in `fetchAndCache` (`server.go:66-68`) for deduplicating upstream fetches is reused for token fetches, preventing concurrent requests for the same scope from each independently hitting the auth endpoint.

Token caching follows the same lifecycle pattern as the S3 cache: check for existing entry, use if valid, fetch if missing or expired.

No existing code in the codebase handles OCI-specific logic. This design introduces OCI awareness at the transport layer only — the rest of the proxy remains protocol-agnostic.

## Implementation Phases

<!-- START_PHASE_1 -->
### Phase 1: Challenge parsing and token types

**Goal:** Parse OCI Bearer challenges and define token types.

**Components:**
- `oci_auth.go` — `ociAuthChallenge` struct, `isOCIAuthChallenge()`, `parseOCIAuthChallenge()` functions
- `oci_auth_test.go` — table-driven tests for challenge parsing (valid, malformed, non-Bearer, missing fields, multiple WWW-Authenticate headers, case-insensitive scheme/keys, quoted values)

**Dependencies:** None

**Done when:** Challenge parsing correctly handles valid OCI challenges, rejects non-OCI 401s, and all parsing tests pass. Covers `oci-token-auth.AC1.1`, `oci-token-auth.AC1.2`, `oci-token-auth.AC3.1`, `oci-token-auth.AC3.2`.
<!-- END_PHASE_1 -->

<!-- START_PHASE_2 -->
### Phase 2: Token cache

**Goal:** In-memory token cache with TTL and concurrency safety.

**Components:**
- `oci_auth.go` — `tokenCache` struct with `get`, `set` methods, expiry buffer
- `oci_auth_test.go` — cache expiry, concurrent access, TTL buffer tests

**Dependencies:** Phase 1 (token types)

**Done when:** Cache stores and retrieves tokens by key, respects TTL with 30-second buffer, is safe for concurrent use. Covers `oci-token-auth.AC2.1`, `oci-token-auth.AC2.2`, `oci-token-auth.AC2.3`.
<!-- END_PHASE_2 -->

<!-- START_PHASE_3 -->
### Phase 3: Token fetching with singleflight

**Goal:** Fetch anonymous tokens from auth endpoints, deduplicated by scope.

**Components:**
- `oci_auth.go` — token fetch function using base transport, JSON parsing, singleflight group
- `oci_auth_test.go` — successful fetch, fetch failure, singleflight deduplication (concurrent requests to same scope), JSON parse error handling, `access_token` field fallback, missing `expires_in` default

**Dependencies:** Phase 1 (challenge types), Phase 2 (token cache)

**Done when:** Token fetch works against test server, errors propagate cleanly, concurrent fetches for same scope are deduplicated. Covers `oci-token-auth.AC1.3`, `oci-token-auth.AC2.4`, `oci-token-auth.AC3.3`, `oci-token-auth.AC3.4`.
<!-- END_PHASE_3 -->

<!-- START_PHASE_4 -->
### Phase 4: ociAuthTransport

**Goal:** RoundTripper that handles discovery, proactive attachment, and refresh.

**Components:**
- `oci_auth.go` — `ociAuthTransport` struct implementing `http.RoundTripper`, wiring challenge detection, token cache, URL-to-scope mapping, and fetch into the three-path flow (proactive, discovery, refresh). Stale token handling: if a proactive token yields 401, evict the token and fall through to the discovery path (one re-discovery, no loop).
- `oci_auth_test.go` — transport-level tests: first request discovery (two upstream calls), subsequent request proactive (one upstream call), token refresh on near-expiry, non-OCI 401 passthrough, token fetch failure returns upstream 401, stale/revoked token triggers re-discovery, request with existing Authorization header bypasses OCI logic

**Dependencies:** Phases 1-3

**Done when:** Transport transparently resolves OCI Bearer challenges. Tests verify all three paths (proactive, discovery, refresh), error cases, and bypass behavior. Covers `oci-token-auth.AC1.4`, `oci-token-auth.AC1.5`, `oci-token-auth.AC1.6`, `oci-token-auth.AC3.5`, `oci-token-auth.AC3.6`.
<!-- END_PHASE_4 -->

<!-- START_PHASE_5 -->
### Phase 5: Wire transport and update integration tests

**Goal:** Connect `ociAuthTransport` in `main.go` and update existing integration tests to verify end-to-end behavior.

**Components:**
- `main.go` — wrap existing transport with `ociAuthTransport` before passing to `http.Client`
- `server_test.go` — update `DockerHubAuthPath_RelaysWWWAuthenticate` and `DockerHubAuthPath_ForwardsClientAuth` to test correct end-to-end behavior (client sends bare request, gets 303 redirect, never sees 401). Add cache revalidation integration test.
- `server_test.go` — update `newTestServer` / `newTestServerWithFallback` helpers to wire `ociAuthTransport` into test setup

**Dependencies:** Phase 4

**Done when:** Integration tests pass showing the full proxy handles auth-required registries transparently. Both initial fetch and cache revalidation work. Covers `oci-token-auth.AC1.7`, `oci-token-auth.AC1.8`, `oci-token-auth.AC4.1`, `oci-token-auth.AC4.2`.
<!-- END_PHASE_5 -->

## Additional Considerations

**Token scope is per-repository.** A token for `repository:library/amazonlinux:pull` does not authorize `repository:library/ubuntu:pull`. The token cache keys on the full `(realm, service, scope)` tuple. Switching repositories on the same host incurs one discovery round trip.

**No retry loops.** The transport attempts at most one resend per request (after fetching a token). If the resend also returns 401, that response surfaces to the caller. This prevents infinite loops from misconfigured registries.

**Rate limits.** Docker Hub allows 100 anonymous pulls per 6 hours per IP. Token fetches are not counted as pulls. The proxy consolidates pulls through singleflight, so N concurrent clients for the same manifest result in 1 upstream pull.

**Challenge map growth.** The challenges map grows by one entry per distinct repository accessed. In practice this is bounded by the set of images the proxy serves (tens to low hundreds). No eviction is needed for the expected scale. If this becomes a concern, an LRU or periodic sweep can be added without changing the transport's external behavior.
