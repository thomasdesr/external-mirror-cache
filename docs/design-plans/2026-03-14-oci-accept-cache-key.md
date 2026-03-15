# OCI Accept-Aware Cache Key Design

## Summary

Mirror-cache currently uses the request URL as the sole cache key. This works for most upstreams, but OCI registries (Docker Hub, gcr.io, etc.) serve multiple distinct wire formats for the same URL, distinguished by the HTTP `Accept` header. A request for an OCI image index and a request for a Docker V2 manifest can arrive at the same URL — and today, whichever format reaches the cache first will be served to all subsequent clients, regardless of what they asked for.

This change makes the cache key Accept-aware for OCI paths. A new `CacheKey` type replaces the bare `*url.URL` throughout the cache interface, adding an opaque `Variant` field. For OCI paths (`/v2/...`), the variant is set to the client's raw `Accept` header value; for all other paths, the variant is empty, preserving existing URL-only keying. The client's `Accept` header is also forwarded transparently to the upstream registry.

## Definition of Done

The OCI adapter forwards the client's Accept header to upstream registries and includes it in the cache key, so that different clients requesting different manifest formats don't collide in cache. Cache key construction becomes adapter-owned: the OCI adapter keys on (URL, Accept), while non-OCI paths retain URL-only keys. The singleflight dedup key matches the cache key. The uncommitted `Accept: */*` hardcoding in server.go is removed.

**Success criteria:**
- OCI adapter forwards client Accept header to upstream registries. No Accept from client = no Accept sent upstream.
- OCI adapter cache key always includes Accept header alongside the URL. No manifest-vs-blob special-casing — the adapter uniformly keys on (URL, Accept).
- Singleflight dedup key matches cache key (URL + Accept) so concurrent requests for different formats aren't collapsed into one fetch.
- Cache key construction is adapter-owned — the OCI adapter decides its keying strategy; non-OCI paths keep URL-only keys.
- The uncommitted `Accept: */*` line in server.go is removed.

**Out of scope:**
- Forwarding client headers other than Accept
- Accept header normalization or sorting
- OCI-specific default Accept headers when client sends none
- Tag-to-digest resolution or manifest list traversal

## Acceptance Criteria

### oci-accept-cache-key.AC1: CacheKey type produces correct S3 paths
- **oci-accept-cache-key.AC1.1 Success:** CacheKey with empty Variant produces S3 path identical to current URL-only path
- **oci-accept-cache-key.AC1.2 Success:** CacheKey with non-empty Variant appends `//` + URL-escaped variant to S3 path
- **oci-accept-cache-key.AC1.3 Edge:** CacheKey with Variant containing special characters (slashes, colons, plus signs) produces valid S3 key via URL escaping

### oci-accept-cache-key.AC2: Accept header forwarding and cache keying
- **oci-accept-cache-key.AC2.1 Success:** OCI path request (`/v2/...`) with Accept header includes Accept in CacheKey.Variant
- **oci-accept-cache-key.AC2.2 Success:** Non-OCI path request produces CacheKey with empty Variant regardless of Accept header
- **oci-accept-cache-key.AC2.3 Success:** Client Accept header is set on upstream request for OCI paths
- **oci-accept-cache-key.AC2.4 Passthrough:** Client request with no Accept header sends no Accept to upstream (proxy does not inject default)

### oci-accept-cache-key.AC3: Singleflight dedup matches cache key
- **oci-accept-cache-key.AC3.1 Success:** Concurrent OCI requests with same URL and same Accept are deduplicated (single upstream fetch)
- **oci-accept-cache-key.AC3.2 Success:** Concurrent OCI requests with same URL but different Accept are NOT deduplicated (separate upstream fetches)

### oci-accept-cache-key.AC4: End-to-end cache isolation
- **oci-accept-cache-key.AC4.1 Success:** Two OCI requests with different Accept headers produce separate cache entries; each returns the correct content type
- **oci-accept-cache-key.AC4.2 Success:** Second request with same Accept hits cache (no upstream fetch, returns presigned URL)
- **oci-accept-cache-key.AC4.3 Success:** Cache revalidation (If-None-Match) works correctly with Accept-keyed entries
- **oci-accept-cache-key.AC4.4 Success:** Non-OCI requests with different Accept headers share cache entry (Accept ignored in key)

## Glossary

- **OCI (Open Container Initiative)**: Industry standard for container image formats and distribution. Defines the registry API (`/v2/...` paths) and multiple manifest wire formats that a single registry endpoint must serve.
- **Accept header**: HTTP request header indicating which response media types the client understands. OCI registries use it to decide whether to return an OCI image index, a Docker V2 manifest, a single-platform manifest, etc.
- **Manifest**: OCI/Docker metadata document describing a container image or image list. Multiple manifest formats (OCI image index, Docker V2 manifest list, single-platform manifest) may be served from the same URL based on Accept.
- **Cache key**: The identifier used to store and retrieve a cached response. Today this is the request URL; after this change, OCI paths key on (URL, Accept).
- **Variant**: The `CacheKey` field that carries Accept-derived differentiation. Empty for non-OCI paths; set to the raw Accept header value for OCI paths. Appended to the S3 path using a `//` separator.
- **Singleflight**: Deduplication primitive that collapses concurrent in-flight requests for the same key into a single upstream fetch. After this change, the singleflight key matches the cache key (URL + Variant) so concurrent requests for different Accept values are not collapsed.
- **Presigned URL**: A time-limited, pre-authenticated S3 URL. The proxy returns a 303 redirect to a presigned URL rather than proxying the response body on cache hits.
- **`httpCache` interface**: The Go interface (`Head`, `Put`, `GetPresignedURL`) that abstracts the cache backend. Currently takes `*url.URL`; this change replaces that parameter with `CacheKey` across all three methods.
- **`ociAuthTransport`**: Existing `http.RoundTripper` that handles OCI Bearer token negotiation (401 challenge-response). Sits downstream of the cache layer; this change is upstream of it in the request flow.
- **`keyFunc`**: A new function field on `cacheMiddleware` that maps (target URL, inbound request) to a `CacheKey`. Introduced as a lightweight extension point rather than a full interface, as the first pre-cache decision point in the proxy.
- **`extractOCIRepository`**: Existing helper in `oci_auth.go` that identifies OCI paths by checking for the `/v2/` prefix and action segments. Reused by `keyFunc` to avoid duplicating path detection logic.

## Architecture

New `CacheKey` type replaces `*url.URL` in the `httpCache` interface, adding an opaque `Variant` field alongside the URL. For OCI paths, the variant is the client's raw Accept header value. For non-OCI paths, the variant is empty (preserving URL-only keying).

A `keyFunc` on `cacheMiddleware` builds the `CacheKey` from the target URL and the inbound client request. Two behaviors exist within a single function: OCI paths (`/v2/...`) include the Accept header as the variant; all other paths produce an empty variant. Selection uses the existing `extractOCIRepository` path check from `oci_auth.go`.

```
Client GET /gcr.io/v2/distroless/base/manifests/latest
       Accept: application/vnd.oci.image.index.v1+json
  │
  ▼
ServeHTTP(w, r)
  │  parseTargetURL(r.URL.Path) → target *url.URL
  │  accept := r.Header.Get("Accept")
  │  key := keyFunc(target, r)        ← OCI: {URL, accept}  non-OCI: {URL, ""}
  │
  ▼
uploadGroup.Do(key.String(), ...)     ← singleflight key: URL+Variant
  │
  ▼
fetchAndCache(ctx, key, accept)
  │  cache.Head(ctx, key)             ← cache lookup by URL+Variant
  │  req := NewRequest(GET, key.URL)
  │  if accept != "" { req.Header.Set("Accept", accept) }
  │  resp := client.Do(req)           ← through ociAuthTransport
  │  cache.Put(ctx, key, resp.Header, body)
  │  cache.GetPresignedURL(ctx, key)
  │
  ▼
303 Redirect → presigned S3 URL
```

**S3 key format:** When the variant is non-empty, `s3PathFor` appends `//` followed by `url.PathEscape(variant)` to the base path. The `//` separator is visually distinct and never appears in normalized URL paths. When the variant is empty, the S3 key is identical to today's format — no migration needed.

```
# No variant (non-OCI or empty Accept):
cache/gcr.io/v2/distroless/base/manifests/latest

# With variant:
cache/gcr.io/v2/distroless/base/manifests/latest//application%2Fvnd.oci.image.index.v1%2Bjson
```

**Accept forwarding:** The client's Accept header is read in `ServeHTTP` and set on the upstream request in `fetchAndCache`. If the client sends no Accept header, none is sent upstream — the proxy does not inject defaults. This means upstream registries that require Accept (like gcr.io for certain manifest types) will return their native error (typically 404), which surfaces to the client.

**Two OCI-aware touch points exist after this change:** the `keyFunc` (pre-cache, new) and `ociAuthTransport` (post-cache, existing). These sit at different layers and serve different concerns. A future design will unify them into a single adapter interface — this change is structured to evolve into that without throwaway work.

### Contract: CacheKey type

```go
type CacheKey struct {
    URL     *url.URL
    Variant string
}

func (k CacheKey) String() string
```

`String()` returns a stable key for singleflight deduplication. When Variant is empty, returns `URL.String()`. When non-empty, returns `URL.String() + "\x00" + Variant` (null-separated to avoid collisions).

### Contract: httpCache interface

```go
type httpCache interface {
    Head(ctx context.Context, key CacheKey) (http.Header, error)
    GetPresignedURL(ctx context.Context, key CacheKey) (string, error)
    Put(ctx context.Context, key CacheKey, headers http.Header, body io.Reader) error
}
```

All three methods change parameter from `*url.URL` to `CacheKey`. The `s3HTTPCache` implementation derives S3 paths from `CacheKey` via updated `s3PathFor`.

### Contract: cacheMiddleware keyFunc

```go
type cacheMiddleware struct {
    cache       httpCache
    client      *http.Client
    fallback    FallbackPolicy
    keyFunc     func(target *url.URL, r *http.Request) CacheKey
    uploadGroup singleflight.Group[string]
}
```

### Contract: fetchAndCache signature

```go
func (m *cacheMiddleware) fetchAndCache(ctx context.Context, key CacheKey, accept string) (string, error)
```

`accept` is passed separately from `CacheKey` because the upstream request uses `key.URL` for the request URL and `accept` for the header — these are consumed at different points.

## Existing Patterns

The `httpCache` interface in `cache.go` uses `*url.URL` as the sole cache key across all three methods. The `s3HTTPCache` in `s3_cache.go` derives S3 paths from URL components in `s3PathFor`. This design changes the key type but preserves the pattern of a single key type flowing through all cache methods.

The singleflight pattern in `server.go` uses `target.String()` as the dedup key. This design changes the key to `CacheKey.String()` but preserves the same pattern.

The `keyFunc` pattern is new — there is no existing adapter-selection or request-routing logic in the proxy. This is the first pre-cache decision point. It is structured as a function field on `cacheMiddleware` rather than an interface to keep the change minimal. A future adapter interface design will replace this with a proper protocol adapter.

`extractOCIRepository` in `oci_auth.go` already identifies OCI paths by checking for `/v2/` prefix and action segments (`/manifests/`, `/blobs/`, `/tags/`). The `keyFunc` reuses this for path detection rather than duplicating the logic.

## Implementation Phases

<!-- START_PHASE_1 -->
### Phase 1: CacheKey type and httpCache interface change

**Goal:** Introduce `CacheKey` type and update the cache interface and implementation to use it.

**Components:**
- `CacheKey` struct and `String()` method in `cache.go`
- `httpCache` interface updated from `*url.URL` to `CacheKey` in `cache.go`
- `s3HTTPCache` updated in `s3_cache.go` — all three methods and `s3PathFor` take `CacheKey`, append `//` + escaped variant when non-empty
- `fakeCache` in `server_test.go` updated to match new interface

**Dependencies:** None

**Done when:** Code compiles. Existing tests pass with `CacheKey{URL: url, Variant: ""}` producing identical behavior to before. Covers `oci-accept-cache-key.AC1.1`, `oci-accept-cache-key.AC1.2`.
<!-- END_PHASE_1 -->

<!-- START_PHASE_2 -->
### Phase 2: keyFunc and Accept forwarding in cacheMiddleware

**Goal:** Wire `keyFunc` into `cacheMiddleware` so OCI paths include Accept in the cache key and forward it to upstream.

**Components:**
- `keyFunc` field on `cacheMiddleware` in `server.go`
- `ServeHTTP` reads client Accept header and calls `keyFunc` to build `CacheKey`
- `fetchAndCache` signature changes to `(ctx, CacheKey, accept string)` — sets Accept on upstream request when non-empty, omits it when empty
- Singleflight key changes from `target.String()` to `key.String()`
- Remove uncommitted `Accept: */*` hardcoding from `server.go`
- Wire `keyFunc` in `main.go` — single function that checks OCI path and includes Accept

**Dependencies:** Phase 1

**Done when:** OCI requests include Accept in cache key and forward it upstream. Non-OCI requests behave identically to before. Singleflight dedup key matches cache key. Covers `oci-accept-cache-key.AC2.1`, `oci-accept-cache-key.AC2.2`, `oci-accept-cache-key.AC2.3`, `oci-accept-cache-key.AC2.4`, `oci-accept-cache-key.AC3.1`, `oci-accept-cache-key.AC3.2`.
<!-- END_PHASE_2 -->

<!-- START_PHASE_3 -->
### Phase 3: Integration tests for Accept-aware caching

**Goal:** Verify end-to-end behavior: different Accept headers produce different cache entries, same Accept reuses cache, non-OCI paths unaffected.

**Components:**
- Integration tests in `server_test.go` — OCI manifest requests with different Accept headers cached separately, same Accept hits cache, cache revalidation works with Accept-keyed entries
- Integration test verifying non-OCI paths ignore Accept in cache key
- Integration test verifying empty Accept (no header from client) produces no Accept on upstream request
- Update existing OCI auth integration tests to set Accept headers on client requests

**Dependencies:** Phase 2

**Done when:** All integration tests pass demonstrating correct cache isolation by Accept header. Covers `oci-accept-cache-key.AC4.1`, `oci-accept-cache-key.AC4.2`, `oci-accept-cache-key.AC4.3`, `oci-accept-cache-key.AC4.4`.
<!-- END_PHASE_3 -->

## Additional Considerations

**Existing cached objects are unaffected.** Non-OCI paths produce `CacheKey{Variant: ""}` which generates the same S3 path as today. No migration needed.

**OCI cache entries from before this change will not be found** because the old entries were stored under URL-only keys and new OCI requests will include Accept in the key. This is acceptable — the proxy will re-fetch and cache under the new key. The old entries will remain as orphans in S3 until manually cleaned up or TTL'd.

**Future adapter interface.** The `keyFunc` pattern is a stepping stone. When a second adapter is needed (or additional OCI concerns arise), the `keyFunc`, Accept forwarding, and `ociAuthTransport` should be unified into a single adapter interface that owns cache key, request preparation, and transport for a given protocol. This is documented here but out of scope for this change.
