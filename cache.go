package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
)

// httpCache defines the interface for caching HTTP responses.
type httpCache interface {
	// Head checks if the URL is cached and returns its headers.
	// Returns (nil, nil) for cache miss.
	Head(ctx context.Context, url *url.URL) (http.Header, error)

	// GetPresignedURL returns a URL to access the cached content.
	GetPresignedURL(ctx context.Context, url *url.URL) (string, error)

	// Put stores the response body and headers for the given URL.
	Put(ctx context.Context, url *url.URL, headers http.Header, body io.Reader) (string, error)
}
