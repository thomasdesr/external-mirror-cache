package main

import (
	"net/http"
)

// injectCacheHeadersIntoRequest updates the provided request with cache
// control headers we're relying on, e.g. just etag & last-modified.
//
// Significantly simplifed version of the logic from here:
// https://github.com/gregjones/httpcache/blob/901d90724c7919163f472a9812253fb26761123d/httpcache.go#L168-L184
func injectCacheHeadersIntoRequest(req *http.Request, cachedHeaders http.Header) {
	etag := cachedHeaders.Get("etag")
	if etag != "" && req.Header.Get("etag") == "" {
		req.Header.Set("if-none-match", etag)
	}

	lastModified := cachedHeaders.Get("last-modified")
	if lastModified != "" && req.Header.Get("last-modified") == "" {
		req.Header.Set("if-modified-since", lastModified)
	}
}
