package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/thomasdesr/external-mirror-cache/internal/errorutil"
	"github.com/thomasdesr/external-mirror-cache/internal/reqlog"
	"github.com/thomasdesr/external-mirror-cache/internal/singleflight"
)

var errInvalidPath = errors.New("invalid path")

// upstreamError represents an HTTP error response from upstream.
// This allows relaying the original status code to clients.
type upstreamError struct {
	StatusCode int
	URL        string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("upstream returned %d for %s", e.StatusCode, e.URL)
}

// cacheMiddleware handles request validation, cache checks, and upstream fetching.
// All responses redirect to cached content in S3.
type cacheMiddleware struct {
	cache       httpCache
	client      *http.Client
	fallback    FallbackPolicy
	keyFunc     func(target *url.URL, r *http.Request) CacheKey
	uploadGroup singleflight.Group[string] // dedupes concurrent requests, returns presigned URL
}

func (m *cacheMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)

		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Only GET requests are supported", http.StatusMethodNotAllowed)

		return
	}

	target, err := parseTargetURL(r.URL.Path, r.URL.RawQuery)
	if err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %s", err), http.StatusBadRequest)

		return
	}

	accept := acceptHeader(r)
	key := m.buildKey(target, r)

	// Singleflight ensures only one request fetches from upstream.
	// All callers (including leader) redirect to the cached content.
	// Use detached context so client disconnects don't abort fetches
	// that other singleflight waiters depend on.
	//nolint:contextcheck // intentional detached context, see comment above
	presignedURL, err, _ := m.uploadGroup.Do(key.String(), func() (string, error) {
		return m.fetchAndCache(context.WithoutCancel(r.Context()), key, accept)
	})
	if err != nil {
		logger := reqlog.FromContext(r.Context())
		logger.Error("failed to fetch and cache", "target", target.String(), "error", err)

		var ue *upstreamError
		if errors.As(err, &ue) {
			http.Error(w, http.StatusText(ue.StatusCode), ue.StatusCode)
		} else {
			// Not http.StatusText(502): a relayed upstream 502 renders as
			// "Bad Gateway" above, and a mirror-side failure (S3 write,
			// upstream transport) must be distinguishable from it, or clients
			// misread internal failures as upstream outages.
			http.Error(w, "mirror-cache error: upstream fetch or cache write failed", http.StatusBadGateway)
		}

		return
	}

	http.Redirect(w, r, presignedURL, http.StatusSeeOther)
}

// fetchAndCache fetches from upstream and caches to S3, returning the presigned URL.
// Skips re-upload when upstream confirms the cached content is current: a 304,
// or a 200 whose strong ETag matches the cached one.
func (m *cacheMiddleware) fetchAndCache(ctx context.Context, key CacheKey, accept string) (string, error) {
	logger := reqlog.FromContext(ctx)
	// Check cache for conditional request headers
	cachedHeaders, err := m.cache.Head(ctx, key)
	if err != nil {
		logger.Warn("cache head error", "target", key.URL.String(), "error", err)

		cachedHeaders = nil
	}

	conditionalFetch := cachedHeaders != nil

	fetchType := "first"
	if conditionalFetch {
		fetchType = "conditional"
	}

	req, err := buildUpstreamRequest(ctx, key.URL, accept, cachedHeaders)
	if err != nil {
		return "", err
	}

	logger = logger.With(
		slog.String("target", key.URL.String()),
		slog.String("fetch", fetchType),
		reqlog.HeaderAttrs("upstream_request_headers", req.Header),
	)

	logger.Debug("fetching upstream")

	resp, err := m.client.Do(req)
	if err != nil {
		if conditionalFetch && m.fallback.ShouldFallback(err, 0) {
			logger.Warn("upstream response", "status", 0, "action", "stale", "action_reason", err.Error())

			return m.presign(ctx, key)
		}

		return "", errorutil.Wrapf(err, "fetch %s (no cache available)", key.URL)
	}

	defer resp.Body.Close() //nolint:errcheck // best-effort close

	logger = logger.With(reqlog.HeaderAttrs("upstream_response_headers", resp.Header))

	// Content already cached: a true 304, or a 200 whose strong ETag matches
	// the cached one. Return without touching resp.Body so the deferred Close
	// abandons any 200 body instead of downloading it to discard.
	if reason, ok := upstreamUnchanged(resp, cachedHeaders); ok {
		logger.Info("upstream response", "status", resp.StatusCode, "action", "revalidated", "action_reason", reason)

		return m.presign(ctx, key)
	}

	// Non-200 responses - check fallback policy
	if resp.StatusCode != http.StatusOK {
		if conditionalFetch && m.fallback.ShouldFallback(nil, resp.StatusCode) {
			logger.Warn("upstream response", "status", resp.StatusCode, "action", "stale")

			return m.presign(ctx, key)
		}

		actionReason := "no cached content"
		if conditionalFetch {
			actionReason = "fallback policy denied"
		}

		logger.Info("upstream response", "status", resp.StatusCode, "action", "error", "action_reason", actionReason)

		return "", &upstreamError{StatusCode: resp.StatusCode, URL: key.URL.String()}
	}

	// 200 OK - stream to cache
	err = m.cache.Put(ctx, key, resp.Header, bufio.NewReader(resp.Body))
	if err != nil {
		if conditionalFetch && m.fallback.ShouldFallback(err, 0) {
			logger.Warn("cache write failed", "action", "stale", "action_reason", err.Error())

			return m.presign(ctx, key)
		}

		return "", errorutil.Wrapf(err, "cache %s", key.URL)
	}

	if conditionalFetch {
		logger.Info("upstream response", "status", resp.StatusCode, "action", "refreshed", "action_reason", "content modified")
	} else {
		logger.Info("upstream response", "status", resp.StatusCode, "action", "cached")
	}

	return m.presign(ctx, key)
}

// upstreamUnchanged reports whether resp indicates the cached content is
// still current, and the action_reason to log for it. Both cases require a
// cached entry: with nothing cached the request carried no validators, so a
// 304 confirms nothing and the content it claims is current may not exist in
// S3 at all. A 200 whose ETag is a strong match for the cached one counts:
// some upstreams answer a revalidation with a full 200 carrying the very ETag
// we sent rather than a 304 -- files.pythonhosted.org's Fastly VCL strips the
// conditional headers server-side -- and a strong match means the body is
// byte-identical to what is already cached, so re-uploading it would only burn
// S3 PutObject quota.
func upstreamUnchanged(resp *http.Response, cachedHeaders http.Header) (string, bool) {
	switch {
	case resp.StatusCode == http.StatusNotModified && cachedHeaders != nil:
		return "not modified", true
	case resp.StatusCode == http.StatusOK && etagStrongMatch(cachedHeaders.Get("ETag"), resp.Header.Get("ETag")):
		return "etag match on 200", true
	}

	return "", false
}

// etagStrongMatch reports whether two entity tags are equivalent under the
// RFC 9110 §8.8.3.2 strong comparison: both are strong tags and they are
// octet-identical.
func etagStrongMatch(cached, resp string) bool {
	if cached != resp {
		return false
	}

	// A strong entity-tag is DQUOTE-delimited with no weak "W/" prefix
	// (RFC 9110 §8.8.3). Weak or malformed tags -- W/"x", the invalid
	// lowercase w/"x", an unquoted value -- never promise byte identity,
	// so they never strong-match even when octet-identical.
	return len(cached) >= 2 && strings.HasPrefix(cached, `"`) && strings.HasSuffix(cached, `"`)
}

func buildUpstreamRequest(ctx context.Context, target *url.URL, accept string, cachedHeaders http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errorutil.Wrapf(err, "create request for %s", target)
	}

	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	if cachedHeaders != nil {
		injectCacheHeadersIntoRequest(req, cachedHeaders)
	}

	return req, nil
}

func (m *cacheMiddleware) presign(ctx context.Context, key CacheKey) (string, error) {
	u, err := m.cache.GetPresignedURL(ctx, key)
	if err != nil {
		return "", errorutil.Wrapf(err, "presign %s", key.URL)
	}

	return u, nil
}

func (m *cacheMiddleware) buildKey(target *url.URL, r *http.Request) CacheKey {
	if m.keyFunc != nil {
		return m.keyFunc(target, r)
	}

	return CacheKey{URL: target}
}

// ociAwareKeyFunc builds a CacheKey that includes the Accept header as the
// variant for OCI paths (/v2/...), enabling per-format caching. Non-OCI paths
// produce an empty variant, preserving URL-only keying.
func ociAwareKeyFunc(target *url.URL, r *http.Request) CacheKey {
	if _, _, ok := extractOCIRepository(target); ok {
		return CacheKey{URL: target, Variant: acceptHeader(r)}
	}

	return CacheKey{URL: target}
}

// acceptHeader returns the request's complete Accept header set as a single
// comma-joined value. Clients may send multiple Accept headers -- Bazel's
// downloader emits Java's default Accept first and appends the real media-type
// Accept second -- which RFC 9110 §5.3 makes semantically one comma-separated
// header. Reading only the first value (http.Header.Get) drops media types and
// breaks strict content negotiation (nvcr.io 404s OCI manifests). The variant
// and the forwarded value derive from this same join so the cache key matches
// what is sent upstream.
func acceptHeader(r *http.Request) string {
	return strings.Join(r.Header.Values("Accept"), ", ")
}

// parseTargetURL extracts the upstream URL from the request path.
// Path format: /<domain>/<path>.
func parseTargetURL(path, rawQuery string) (*url.URL, error) {
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 2)

	if len(parts) != 2 {
		return nil, errorutil.Wrapf(errInvalidPath, "invalid path %q", path)
	}

	return &url.URL{
		Scheme:   "https",
		Host:     parts[0],
		Path:     "/" + parts[1],
		RawQuery: rawQuery,
	}, nil
}
