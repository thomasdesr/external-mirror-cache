package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
)

// CacheKey identifies a cached response. URL is the upstream URL; Variant
// differentiates entries when the same URL can produce different responses
// (e.g., OCI manifests keyed by Accept header). Empty Variant preserves
// URL-only keying.
type CacheKey struct {
	URL     *url.URL
	Variant string
}

// String returns a stable key for singleflight deduplication.
// Empty Variant returns URL.String() (backward-compatible).
// Non-empty Variant appends a null separator + Variant to avoid collisions.
func (k CacheKey) String() string {
	s := k.URL.String()
	if k.Variant != "" {
		return s + "\x00" + k.Variant
	}

	return s
}

// httpCache defines the interface for caching HTTP responses.
type httpCache interface {
	// Head checks if the key is cached and returns its headers.
	// Returns (nil, nil) for cache miss.
	Head(ctx context.Context, key CacheKey) (http.Header, error)

	// GetPresignedURL returns a URL to access the cached content.
	GetPresignedURL(ctx context.Context, key CacheKey) (string, error)

	// Put stores the response body and headers for the given key.
	Put(ctx context.Context, key CacheKey, headers http.Header, body io.Reader) error
}
