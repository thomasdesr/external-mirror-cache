package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"time"
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
	// Head checks if the key is cached and returns its entry.
	// Returns (nil, nil) for cache miss.
	Head(ctx context.Context, key CacheKey) (*cachedEntry, error)

	// GetPresignedURL returns a URL to access the cached content.
	GetPresignedURL(ctx context.Context, key CacheKey) (string, error)

	// Put stores the response body and headers for the given key.
	Put(ctx context.Context, key CacheKey, headers http.Header, body io.Reader) error

	// Touch re-arms an entry's freshness window after a successful
	// revalidation: it advances the stored-at time and replaces the stored
	// headers with the supplied (already merged) set, body untouched. The
	// write is conditional on the object still matching entry.ObjectETag,
	// and objects too large to copy are skipped, not attempted. Errors are
	// advisory: the caller keeps revalidating, never fails the request.
	Touch(ctx context.Context, key CacheKey, entry *cachedEntry, headers http.Header) error
}

// cachedEntry is a cache hit: the stored response headers plus the
// storage-side facts the freshness gate and conditional touch consume.
type cachedEntry struct {
	Headers http.Header
	// StoredAt is when the object was written (S3 LastModified); the
	// freshness gate anchors age arithmetic to it.
	StoredAt time.Time
	// ObjectETag is the storage object's own ETag — not the stored upstream
	// ETag header. Touch's conditional copy matches against it so a touch
	// racing a concurrent Put loses instead of clobbering the newer object.
	ObjectETag string
	// Size is the stored body's byte length; Touch skips objects above the
	// CopyObject single-part limit.
	Size int64
}
