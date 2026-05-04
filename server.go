package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

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

	// conditionalFetchTimeout bounds the upstream call when we already have
	// cached content to fall back to. Zero leaves the http.Client's own
	// timeouts in charge.
	conditionalFetchTimeout time.Duration
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

	accept := r.Header.Get("Accept")
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
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		}

		return
	}

	http.Redirect(w, r, presignedURL, http.StatusSeeOther)
}

// fetchAndCache fetches from upstream and caches to S3, returning the presigned URL.
// If content is already cached and upstream returns 304, skips re-upload.
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

	logger = logger.With("target", key.URL.String(), "fetch", fetchType)

	fetchCtx := ctx
	if conditionalFetch && m.conditionalFetchTimeout > 0 {
		var cancel context.CancelFunc
		fetchCtx, cancel = context.WithTimeout(ctx, m.conditionalFetchTimeout)
		defer cancel()
	}

	req, err := buildUpstreamRequest(fetchCtx, key.URL, accept, cachedHeaders)
	if err != nil {
		return "", err
	}

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

	// 304 Not Modified - content already cached
	if resp.StatusCode == http.StatusNotModified {
		logger.Info("upstream response", "status", resp.StatusCode, "action", "revalidated")

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
		return "", errorutil.Wrapf(err, "cache %s", key.URL)
	}

	if conditionalFetch {
		logger.Info("upstream response", "status", resp.StatusCode, "action", "refreshed", "action_reason", "content modified")
	} else {
		logger.Info("upstream response", "status", resp.StatusCode, "action", "cached")
	}

	return m.presign(ctx, key)
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
		return CacheKey{URL: target, Variant: r.Header.Get("Accept")}
	}

	return CacheKey{URL: target}
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
