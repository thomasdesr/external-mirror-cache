package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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

	// Singleflight ensures only one request fetches from upstream.
	// All callers (including leader) redirect to the cached content.
	// Use detached context so client disconnects don't abort fetches
	// that other singleflight waiters depend on.
	//nolint:contextcheck // intentional detached context, see comment above
	presignedURL, err, _ := m.uploadGroup.Do(target.String(), func() (string, error) {
		return m.fetchAndCache(context.WithoutCancel(r.Context()), target)
	})
	if err != nil {
		logger := reqlog.FromContext(r.Context())
		logger.Error("failed to fetch and cache", "error", err)

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
func (m *cacheMiddleware) fetchAndCache(ctx context.Context, target *url.URL) (string, error) {
	logger := reqlog.FromContext(ctx)

	// Check cache for conditional request headers
	cachedHeaders, err := m.cache.Head(ctx, target)
	if err != nil {
		logger.Warn("cache head error", "target", target.String(), "error", err)

		cachedHeaders = nil
	}

	// Build upstream request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "", errorutil.Wrapf(err, "create request for %s", target)
	}

	if cachedHeaders != nil {
		injectCacheHeadersIntoRequest(req, cachedHeaders)
	}

	// Fetch from upstream
	logger.Debug("fetching from upstream", "target", target.String(), "has_cached", cachedHeaders != nil)

	resp, err := m.client.Do(req)
	if err != nil {
		if cachedHeaders != nil && m.fallback.ShouldFallback(err, 0) {
			logger.Warn("upstream error, serving stale", "target", target.String(), "error", err)

			return m.presign(ctx, target)
		}

		return "", errorutil.Wrapf(err, "fetch %s (no cache available)", target)
	}

	defer resp.Body.Close() //nolint:errcheck // best-effort close

	// 304 Not Modified - content already cached
	if resp.StatusCode == http.StatusNotModified {
		logger.Info("upstream returned 304, using cached content", "target", target.String())

		return m.presign(ctx, target)
	}

	// Non-200 responses - check fallback policy
	if resp.StatusCode != http.StatusOK {
		if cachedHeaders != nil && m.fallback.ShouldFallback(nil, resp.StatusCode) {
			logger.Warn("upstream error status, serving stale", "target", target.String(), "status", resp.StatusCode)

			return m.presign(ctx, target)
		}

		return "", &upstreamError{StatusCode: resp.StatusCode, URL: target.String()}
	}

	// 200 OK - stream to cache
	err = m.cache.Put(ctx, target, resp.Header, bufio.NewReader(resp.Body))
	if err != nil {
		return "", errorutil.Wrapf(err, "cache %s", target)
	}

	logger.Debug("cached upstream response", "target", target.String())

	return m.presign(ctx, target)
}

func (m *cacheMiddleware) presign(ctx context.Context, target *url.URL) (string, error) {
	u, err := m.cache.GetPresignedURL(ctx, target)
	if err != nil {
		return "", errorutil.Wrapf(err, "presign %s", target)
	}

	return u, nil
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
