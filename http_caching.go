package main

import (
	"net/http"
)

// injectCacheHeadersIntoRequest updates the provided request with cache
// control headers we're relying on, e.g. just etag & last-modified.
//
// Significantly simplified version of the logic from here:
// https://github.com/gregjones/httpcache/blob/901d90724c7919163f472a9812253fb26761123d/httpcache.go#L168-L184
func injectCacheHeadersIntoRequest(req *http.Request, cachedHeaders http.Header) {
	etag := cachedHeaders.Get("etag")
	if etag != "" && req.Header.Get("etag") == "" {
		req.Header.Set("If-None-Match", etag)
	}

	lastModified := cachedHeaders.Get("Last-Modified")
	if lastModified != "" && req.Header.Get("Last-Modified") == "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}
}
