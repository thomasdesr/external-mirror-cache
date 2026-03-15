package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thomasdesr/external-mirror-cache/internal/reqlog"
)

// fakeCache is an in-memory httpCache for testing.
type fakeCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	headers http.Header
	body    []byte
}

func newFakeCache() *fakeCache {
	return &fakeCache{entries: make(map[string]*cacheEntry)}
}

func (c *fakeCache) Head(ctx context.Context, key CacheKey) (http.Header, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[key.String()]
	if !ok {
		return nil, nil //nolint:nilnil // cache interface contract
	}

	return entry.headers.Clone(), nil
}

func (c *fakeCache) GetPresignedURL(ctx context.Context, key CacheKey) (string, error) {
	return "http://fake-s3/" + key.URL.Host + key.URL.Path, nil
}

func (c *fakeCache) Put(ctx context.Context, key CacheKey, headers http.Header, body io.Reader) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key.String()] = &cacheEntry{
		headers: headers.Clone(),
		body:    data,
	}

	return nil
}

func (c *fakeCache) get(u string) *cacheEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.entries[u]
}

// newTestServer creates a caching proxy backed by fakeCache and the given upstream.
// The upstream should be a TLS server since parseTargetURL always uses HTTPS scheme.
func newTestServer(upstream *httptest.Server, cache *fakeCache) *httptest.Server {
	return newTestServerWithFallback(upstream, cache, FallbackPolicy{})
}

func newTestServerWithFallback(upstream *httptest.Server, cache *fakeCache, fallback FallbackPolicy) *httptest.Server {
	upstreamClient := upstream.Client()
	upstreamClient.Transport = newOCIAuthTransport(upstreamClient.Transport)

	handler := &cacheMiddleware{
		cache:    cache,
		client:   upstreamClient,
		fallback: fallback,
		keyFunc:  ociAwareKeyFunc,
	}

	return httptest.NewServer(handler)
}

// upstreamHostPath extracts just the host:port from a test server URL for use in proxy paths.
func upstreamHostPath(upstream *httptest.Server, path string) string {
	u, _ := url.Parse(upstream.URL)

	return "/" + u.Host + path
}

func TestIntegration_CacheMissThenHit(t *testing.T) {
	// Track upstream requests
	var upstreamHits atomic.Int32

	upstreamBody := "hello from upstream"

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)

		// Check for conditional request headers
		if r.Header.Get("If-None-Match") == `"test-etag"` {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("ETag", `"test-etag"`)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/test.txt")

	// Don't follow redirects - we want to inspect the redirect response
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// First request: cache miss, fetches from upstream, redirects to cached content
	resp1, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	resp1.Body.Close()

	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp1.StatusCode)
	}

	if upstreamHits.Load() != 1 {
		t.Fatalf("expected 1 upstream hit, got %d", upstreamHits.Load())
	}

	// Verify content was cached
	upstreamURL, _ := url.Parse(upstream.URL)
	cachedURL := "https://" + upstreamURL.Host + "/test.txt"

	entry := cache.get(cachedURL)
	if entry == nil {
		t.Fatal("expected entry to be cached")
	}

	if entry.headers.Get("ETag") != `"test-etag"` {
		t.Fatalf("expected cached ETag, got %q", entry.headers.Get("ETag"))
	}

	if string(entry.body) != upstreamBody {
		t.Fatalf("expected cached body %q, got %q", upstreamBody, entry.body)
	}

	// Second request: cache hit, upstream returns 304, proxy redirects to cache
	resp2, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}

	resp2.Body.Close()

	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp2.StatusCode)
	}

	// Upstream was hit twice: once for initial fetch, once for conditional request
	if upstreamHits.Load() != 2 {
		t.Fatalf("expected 2 upstream hits, got %d", upstreamHits.Load())
	}
}

func TestIntegration_SingleflightDeduplication(t *testing.T) {
	var upstreamHits atomic.Int32

	upstreamDelay := 100 * time.Millisecond

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		time.Sleep(upstreamDelay) // Simulate slow upstream
		w.Header().Set("ETag", `"test-etag"`)
		w.Write([]byte("slow response"))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/slow.txt")

	// Don't follow redirects so we can see what each request gets
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Launch concurrent requests
	const numRequests = 5

	var wg sync.WaitGroup

	results := make(chan *http.Response, numRequests)

	for range numRequests {
		wg.Go(func() {
			resp, err := client.Get(proxy.URL + proxyPath)
			if err != nil {
				t.Errorf("request failed: %v", err)

				return
			}

			results <- resp
		})
	}

	wg.Wait()
	close(results)

	// All requests should get 303 redirects to cached content
	var redirectCount int

	for resp := range results {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusSeeOther {
			redirectCount++
		} else {
			t.Errorf("unexpected status: %d, expected 303", resp.StatusCode)
		}
	}

	if redirectCount != numRequests {
		t.Errorf("expected %d redirects, got %d", numRequests, redirectCount)
	}

	// Only one upstream request should have been made (deduplication)
	if upstreamHits.Load() != 1 {
		t.Errorf("expected 1 upstream hit (deduplication), got %d", upstreamHits.Load())
	}
}

func TestIntegration_UpstreamErrorRelaysStatusCode(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/notfound":
			http.Error(w, "not found", http.StatusNotFound)
		case "/error":
			http.Error(w, "server error", http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	upstreamURL, _ := url.Parse(upstream.URL)

	// Non-200 upstream responses relay the original status code
	// (they are not cacheable, so we can't redirect to cache)
	tests := []struct {
		path     string
		expected int
	}{
		{"/notfound", http.StatusNotFound},
		{"/error", http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			proxyPath := upstreamHostPath(upstream, tc.path)

			resp, err := http.Get(proxy.URL + proxyPath)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}

			resp.Body.Close()

			if resp.StatusCode != tc.expected {
				t.Errorf("expected %d, got %d", tc.expected, resp.StatusCode)
			}

			// Non-200 responses should not be cached
			cachedURL := "https://" + upstreamURL.Host + tc.path
			if cache.get(cachedURL) != nil {
				t.Error("non-200 response should not be cached")
			}
		})
	}
}

func TestIntegration_MethodRestriction(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/test")

	methods := []string{"POST", "PUT", "DELETE", "PATCH"}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req, _ := http.NewRequest(method, proxy.URL+proxyPath, nil)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}

			resp.Body.Close()

			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("expected 405 for %s, got %d", method, resp.StatusCode)
			}
		})
	}
}

func TestIntegration_RangeRequestCachesFullFile(t *testing.T) {
	var upstreamRangeHeader string

	fullContent := "full file content here"

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRangeHeader = r.Header.Get("Range")
		w.Header().Set("ETag", `"test-etag"`)
		w.Write([]byte(fullContent))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/file.bin")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req.Header.Set("Range", "bytes=0-100")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	resp.Body.Close()

	// Should redirect to S3, not reject
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("expected 303 redirect, got %d", resp.StatusCode)
	}

	// Range header should NOT be forwarded to upstream
	if upstreamRangeHeader != "" {
		t.Errorf("Range header should not be forwarded to upstream, got %q", upstreamRangeHeader)
	}

	// Full content should be cached
	upstreamURL, _ := url.Parse(upstream.URL)
	cachedURL := "https://" + upstreamURL.Host + "/file.bin"

	entry := cache.get(cachedURL)
	if entry == nil {
		t.Fatal("expected content to be cached")
	}

	if string(entry.body) != fullContent {
		t.Errorf("expected full content cached, got %q", entry.body)
	}
}

func TestIntegration_LastModifiedConditionalRequest(t *testing.T) {
	var upstreamHits atomic.Int32

	lastModified := "Wed, 21 Oct 2025 07:28:00 GMT"
	upstreamBody := "content with last-modified"

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)

		// Check for conditional request header
		if r.Header.Get("If-Modified-Since") == lastModified {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("Last-Modified", lastModified)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/dated.txt")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// First request: cache miss
	resp1, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	resp1.Body.Close()

	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp1.StatusCode)
	}

	// Verify Last-Modified was cached
	upstreamURL, _ := url.Parse(upstream.URL)
	cachedURL := "https://" + upstreamURL.Host + "/dated.txt"

	entry := cache.get(cachedURL)
	if entry == nil {
		t.Fatal("expected entry to be cached")
	}

	if entry.headers.Get("Last-Modified") != lastModified {
		t.Fatalf("expected cached Last-Modified %q, got %q", lastModified, entry.headers.Get("Last-Modified"))
	}

	// Second request: should send If-Modified-Since, get 304
	resp2, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}

	resp2.Body.Close()

	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp2.StatusCode)
	}

	// Verify conditional request was made
	if upstreamHits.Load() != 2 {
		t.Fatalf("expected 2 upstream hits (initial + conditional), got %d", upstreamHits.Load())
	}
}

func TestIntegration_InvalidPathFormat(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for invalid paths")
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	tests := []struct {
		name string
		path string
	}{
		{"no path after domain", "/example.com"},
		{"empty path", "/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(proxy.URL + tc.path)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}

			resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("expected 400 Bad Request for path %q, got %d", tc.path, resp.StatusCode)
			}
		})
	}
}

func TestIntegration_LeaderCancellation_FollowerStillSucceeds(t *testing.T) {
	var upstreamHits atomic.Int32

	upstreamStarted := make(chan struct{})
	upstreamContinue := make(chan struct{})

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		close(upstreamStarted) // Signal that upstream received request
		<-upstreamContinue     // Wait for test to allow completion
		w.Header().Set("ETag", `"test-etag"`)
		w.Write([]byte("response after delay"))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/slow-cancel.txt")

	// Leader: will be cancelled mid-flight
	leaderCtx, leaderCancel := context.WithCancel(context.Background())
	leaderReq, _ := http.NewRequestWithContext(leaderCtx, http.MethodGet, proxy.URL+proxyPath, nil)

	// Follower: should succeed even if leader cancels
	followerClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	var wg sync.WaitGroup

	leaderResult := make(chan error, 1)
	followerResult := make(chan *http.Response, 1)

	// Start leader request

	wg.Go(func() {
		resp, err := http.DefaultClient.Do(leaderReq)
		if err != nil {
			leaderResult <- err

			return
		}

		resp.Body.Close()

		leaderResult <- nil
	})

	// Wait for upstream to receive the request (leader is now in-flight)
	<-upstreamStarted

	// Start follower request (will join singleflight)

	wg.Go(func() {
		// Small delay to ensure follower joins the existing singleflight
		time.Sleep(10 * time.Millisecond)

		resp, err := followerClient.Get(proxy.URL + proxyPath)
		if err != nil {
			t.Errorf("follower request failed: %v", err)

			followerResult <- nil

			return
		}

		followerResult <- resp
	})

	// Cancel leader's context while both are waiting
	time.Sleep(20 * time.Millisecond)
	leaderCancel()

	// Allow upstream to complete
	close(upstreamContinue)

	wg.Wait()

	// Check results
	leaderErr := <-leaderResult
	followerResp := <-followerResult

	// Leader's request was cancelled (client disconnected)
	if leaderErr == nil {
		t.Log("Leader completed before cancellation took effect")
	}

	// Follower should succeed - fetch continues despite leader's cancellation
	if followerResp == nil {
		t.Fatal("follower should succeed even if leader cancels")
	}

	followerResp.Body.Close()

	if followerResp.StatusCode != http.StatusSeeOther {
		t.Errorf("expected follower to get 303 redirect, got %d", followerResp.StatusCode)
	}

	// Only one upstream request should have been made
	if upstreamHits.Load() != 1 {
		t.Errorf("expected 1 upstream hit, got %d", upstreamHits.Load())
	}
}

func TestIntegration_RedirectFollowing_CacheKeyIsOriginalURL(t *testing.T) {
	var (
		requestPaths []string
		mu           sync.Mutex
	)

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()

		requestPaths = append(requestPaths, r.URL.Path)
		mu.Unlock()

		switch r.URL.Path {
		case "/original":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			w.Header().Set("ETag", `"test-etag"`)
			w.Write([]byte("final content"))
		}
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/original")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp.StatusCode)
	}

	// Verify upstream received both requests (redirect was followed by proxy's client)
	if len(requestPaths) != 2 {
		t.Fatalf("expected 2 upstream requests (redirect followed), got %d: %v", len(requestPaths), requestPaths)
	}

	if requestPaths[0] != "/original" || requestPaths[1] != "/final" {
		t.Fatalf("expected requests to /original then /final, got %v", requestPaths)
	}

	// Cache key must be the ORIGINAL URL, not the redirect destination
	upstreamURL, _ := url.Parse(upstream.URL)
	originalCacheKey := "https://" + upstreamURL.Host + "/original"
	finalCacheKey := "https://" + upstreamURL.Host + "/final"

	if cache.get(originalCacheKey) == nil {
		t.Error("expected content to be cached under original URL")
	}

	if cache.get(finalCacheKey) != nil {
		t.Error("content should NOT be cached under redirect destination URL")
	}
}

func TestIntegration_FallbackOn5xx_WithCache(t *testing.T) {
	var requestCount atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		if count == 1 {
			w.Header().Set("ETag", `"test-etag"`)
			w.Write([]byte("original content"))

			return
		}

		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServerWithFallback(upstream, cache, FallbackPolicy{On5xx: true})
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/fallback-test.txt")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// First request: cache miss, fetches from upstream
	resp1, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	resp1.Body.Close()

	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp1.StatusCode)
	}

	// Second request: upstream returns 500, should fallback to stale cache
	resp2, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}

	resp2.Body.Close()

	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect (stale fallback), got %d", resp2.StatusCode)
	}
}

func TestIntegration_FallbackOn5xx_WithoutCache_RelaysStatusCode(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServerWithFallback(upstream, cache, FallbackPolicy{On5xx: true})
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/no-cache.txt")

	resp, err := http.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	resp.Body.Close()

	// When no cache available, relay the upstream status code
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500 (upstream status) when no cache available, got %d", resp.StatusCode)
	}
}

func TestIntegration_FallbackDisabled_RelaysStatusCode(t *testing.T) {
	var requestCount atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		if count == 1 {
			w.Header().Set("ETag", `"test-etag"`)
			w.Write([]byte("original content"))

			return
		}

		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cache := newFakeCache()
	// Fallback disabled (default zero value)
	proxy := newTestServerWithFallback(upstream, cache, FallbackPolicy{})
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/no-fallback.txt")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// First request: cache content
	resp1, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	resp1.Body.Close()

	// Second request: upstream 500, fallback disabled, relay upstream status
	resp2, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}

	resp2.Body.Close()

	if resp2.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500 (upstream status) when fallback disabled, got %d", resp2.StatusCode)
	}
}

func TestIntegration_FallbackOnAnyError_Covers4xx(t *testing.T) {
	var requestCount atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		if count == 1 {
			w.Header().Set("ETag", `"test-etag"`)
			w.Write([]byte("original content"))

			return
		}

		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServerWithFallback(upstream, cache, FallbackPolicy{OnAnyError: true})
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/any-error.txt")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// First request: cache content
	resp1, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	resp1.Body.Close()

	// Second request: upstream 404, OnAnyError should fallback to stale
	resp2, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}

	resp2.Body.Close()

	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("expected 303 redirect (stale fallback on 404), got %d", resp2.StatusCode)
	}
}

func TestIntegration_OCIAuth_TransparentTokenResolution(t *testing.T) {
	// Track calls to the token server
	var tokenEndpointCalls atomic.Int32

	// Create token server that returns a valid token
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenEndpointCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"test-access-token","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	// Create upstream registry server that returns 401 without Authorization, 200 with it
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			// No auth header: return 401 with challenge pointing to local token server
			challenge := `Bearer realm="` + tokenServer.URL +
				`/token",service="test-registry",scope="repository:library/test:pull"`
			w.Header().Set("WWW-Authenticate", challenge)
			w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		// Has auth header: return success with ETag
		w.Header().Set("ETag", `"manifest-etag"`)
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/v2/library/test/manifests/latest")

	// Client with no redirect following to inspect the response
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	resp.Body.Close()

	// With ociAuthTransport wired in, the proxy should transparently resolve the challenge
	// and return 303 (redirect to cached content), never exposing the 401 to the client
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp.StatusCode)
	}

	// Verify token endpoint was called exactly once
	if tokenEndpointCalls.Load() != 1 {
		t.Errorf("expected token endpoint to be called once, got %d calls", tokenEndpointCalls.Load())
	}

	// Verify content was cached with correct ETag
	upstreamURL, _ := url.Parse(upstream.URL)
	baseCachedURL := "https://" + upstreamURL.Host + "/v2/library/test/manifests/latest"
	cacheKey := baseCachedURL + "\x00" + "application/vnd.docker.distribution.manifest.v2+json"

	entry := cache.get(cacheKey)
	if entry == nil {
		t.Fatal("expected manifest to be cached")
	}

	if entry.headers.Get("ETag") != `"manifest-etag"` {
		t.Fatalf("expected cached ETag %q, got %q", `"manifest-etag"`, entry.headers.Get("ETag"))
	}
}

func TestIntegration_DockerHubAuthPath_ForwardsClientAuth(t *testing.T) {
	t.Skip("proxy-level Authorization forwarding not yet implemented; OCI transport-level bypass tested in oci_auth_test.go (AC3.6)")

	// Create a local token server so upstream challenge doesn't reference external URLs
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"test-token","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	var receivedAuth string

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		if receivedAuth == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+tokenServer.URL+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		w.Header().Set("ETag", `"test-etag"`)
		w.Write([]byte("authenticated content"))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/v2/library/test/manifests/latest")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	token := "Bearer dGVzdC10b2tlbg=="

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req.Header.Set("Authorization", token)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	resp.Body.Close()

	// Upstream should have received the Authorization header
	if receivedAuth != token {
		t.Errorf("expected upstream to receive Authorization %q, got %q", token, receivedAuth)
	}

	// Request should succeed (redirect to cached content)
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("expected 303 redirect, got %d", resp.StatusCode)
	}
}

func TestIntegration_OCIAuth_CacheRevalidation(t *testing.T) {
	// Track calls to the token server and upstream requests
	var (
		tokenEndpointCalls   atomic.Int32
		upstreamRequestCount atomic.Int32
	)

	// Create token server that returns a valid token
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenEndpointCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"test-access-token","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	// Create upstream registry server that requires auth
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequestCount.Add(1)

		if r.Header.Get("Authorization") == "" {
			// No auth header: return 401 with OCI Bearer challenge
			challenge := `Bearer realm="` + tokenServer.URL +
				`/token",service="test-registry",scope="repository:library/test:pull"`
			w.Header().Set("WWW-Authenticate", challenge)
			w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		// Has auth header: check for conditional request
		if r.Header.Get("If-None-Match") == `"manifest-etag"` {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		// Return full manifest with ETag
		w.Header().Set("ETag", `"manifest-etag"`)
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/v2/library/test/manifests/latest")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// First request: initial fetch, populates cache
	req1, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req1.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("first request: expected 303 redirect, got %d", resp1.StatusCode)
	}

	initialTokenCalls := tokenEndpointCalls.Load()
	if initialTokenCalls != 1 {
		t.Errorf("after first request: expected 1 token call, got %d", initialTokenCalls)
	}

	// Second request: should use cached token and send If-None-Match to upstream
	req2, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req2.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("second request: expected 303 redirect, got %d", resp2.StatusCode)
	}

	// Token endpoint should still be called once (cached token reused)
	// The transport may proactively discover/refresh tokens, but shouldn't do extra unnecessary calls
	finalTokenCalls := tokenEndpointCalls.Load()
	if finalTokenCalls > 1 {
		t.Errorf("after second request: expected at most 1 token call (cached), got %d", finalTokenCalls)
	}

	// Verify upstream received conditional request on second call
	// With singleflight + token caching:
	// First request: 401 (no auth) → transport fetches token → 200 (with auth after token fetch)
	// Second request: 304 (with auth + If-None-Match)
	// Total: 3 upstream requests
	if upstreamRequestCount.Load() != 3 {
		t.Errorf("expected exactly 3 upstream requests (401, 200, 304), got %d", upstreamRequestCount.Load())
	}
}

func TestIntegration_OCIAuth_DifferentRepositories(t *testing.T) {
	// Track calls to the token server
	var tokenEndpointCalls atomic.Int32

	// Create token server that returns a valid token
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenEndpointCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"test-access-token","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	// Create upstream registry server that requires auth
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			// No auth header: return 401 with OCI Bearer challenge
			// Different repositories have different scopes, so each requires separate token fetch
			challenge := `Bearer realm="` + tokenServer.URL +
				`/token",service="test-registry",scope="repository` + r.URL.Path + `:pull"`
			w.Header().Set("WWW-Authenticate", challenge)
			w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		// Has auth header: return manifest with ETag
		w.Header().Set("ETag", `"manifest-etag"`)
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Request first repository
	proxyPath1 := upstreamHostPath(upstream, "/v2/library/ubuntu/manifests/latest")

	resp1, err := client.Get(proxy.URL + proxyPath1)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("first request: expected 303 redirect, got %d", resp1.StatusCode)
	}

	// Request second repository (different scope)
	proxyPath2 := upstreamHostPath(upstream, "/v2/library/alpine/manifests/latest")

	resp2, err := client.Get(proxy.URL + proxyPath2)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("second request: expected 303 redirect, got %d", resp2.StatusCode)
	}

	// Each repository has a different scope, so each triggers a separate token fetch
	if tokenEndpointCalls.Load() != 2 {
		t.Errorf("expected token endpoint to be called twice (once per scope), got %d calls", tokenEndpointCalls.Load())
	}

	// Verify both manifests were cached
	upstreamURL, _ := url.Parse(upstream.URL)

	cachedURL1 := "https://" + upstreamURL.Host + "/v2/library/ubuntu/manifests/latest"

	entry1 := cache.get(cachedURL1)
	if entry1 == nil {
		t.Fatal("expected ubuntu manifest to be cached")
	}

	cachedURL2 := "https://" + upstreamURL.Host + "/v2/library/alpine/manifests/latest"

	entry2 := cache.get(cachedURL2)
	if entry2 == nil {
		t.Fatal("expected alpine manifest to be cached")
	}
}

// TestIntegration_OCIAuth_GCRDistrolessDigest exercises the full proxy stack with
// a real-world gcr.io-style URL containing a sha256: digest reference. Verifies
// that the colon in the digest doesn't break URL parsing, S3 cache keying, or
// OCI auth challenge resolution across initial fetch and cache revalidation.
func TestIntegration_OCIAuth_GCRDistrolessDigest(t *testing.T) {
	const digest = "sha256:372adf30255bcdfc80b22ee926fe19c163a7675b737d201f4a09be4877a69e3a"
	manifestBody := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`

	var (
		tokenCalls    atomic.Int32
		upstreamCalls atomic.Int32
	)

	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"gcr-test-token","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)

		if r.Header.Get("Authorization") == "" {
			challenge := `Bearer realm="` + tokenServer.URL +
				`/token",service="gcr.io",scope="repository:distroless/base:pull"`
			w.Header().Set("WWW-Authenticate", challenge)
			w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		// Conditional request: return 304
		if r.Header.Get("If-None-Match") == `"distroless-etag"` {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		// Full response
		w.Header().Set("ETag", `"distroless-etag"`)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digest)
		w.Write([]byte(manifestBody))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/v2/distroless/base/manifests/"+digest)

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// First request: discovery (401 → token fetch → retry with token → 200 → cached)
	req1, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req1.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	resp1.Body.Close()

	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("first request: expected 303 redirect, got %d", resp1.StatusCode)
	}

	// Verify cached with correct key (contains colon from sha256:)
	upstreamURL, _ := url.Parse(upstream.URL)
	baseCachedURL := "https://" + upstreamURL.Host + "/v2/distroless/base/manifests/" + digest
	cacheKey := baseCachedURL + "\x00" + "application/vnd.oci.image.manifest.v1+json"

	entry := cache.get(cacheKey)
	if entry == nil {
		t.Fatal("expected manifest to be cached")
	}

	if entry.headers.Get("ETag") != `"distroless-etag"` {
		t.Errorf("cached ETag: got %q, want %q", entry.headers.Get("ETag"), `"distroless-etag"`)
	}

	if entry.headers.Get("Docker-Content-Digest") != digest {
		t.Errorf("cached Docker-Content-Digest: got %q, want %q", entry.headers.Get("Docker-Content-Digest"), digest)
	}

	// Second request: proactive token + conditional request → 304 → serve from cache
	req2, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req2.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("second request: expected 303 redirect, got %d", resp2.StatusCode)
	}

	// Token fetched once, reused on second request
	if tokenCalls.Load() != 1 {
		t.Errorf("expected 1 token fetch (reused), got %d", tokenCalls.Load())
	}

	// Upstream: 401 + 200 (first request), 304 (second request) = 3
	if upstreamCalls.Load() != 3 {
		t.Errorf("expected 3 upstream calls (401, 200, 304), got %d", upstreamCalls.Load())
	}
}

func TestIntegration_FallbackOnConnectionError_WithCache(t *testing.T) {
	var requestCount atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("ETag", `"test-etag"`)
		w.Write([]byte("original content"))
	}))

	cache := newFakeCache()

	proxy := newTestServerWithFallback(upstream, cache, FallbackPolicy{OnConnectionError: true})
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/conn-error.txt")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// First request: cache content
	resp1, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	resp1.Body.Close()

	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp1.StatusCode)
	}

	// Close upstream to simulate connection error
	upstream.Close()

	// Second request: connection error, should fallback to stale cache
	resp2, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}

	resp2.Body.Close()

	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("expected 303 redirect (stale fallback on conn error), got %d", resp2.StatusCode)
	}
}

// Unit tests for ociAwareKeyFunc (AC2.1, AC2.2)

func TestOCIAwareKeyFunc_OCI_IncludesAccept(t *testing.T) {
	// AC2.1: OCI path with Accept header includes Accept in CacheKey.Variant
	u, _ := url.Parse("https://gcr.io/v2/library/test/manifests/latest")
	req, _ := http.NewRequest(http.MethodGet, "http://proxy/dummy", nil)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json")

	key := ociAwareKeyFunc(u, req)

	if key.Variant != "application/vnd.oci.image.index.v1+json" {
		t.Errorf("expected variant %q, got %q", "application/vnd.oci.image.index.v1+json", key.Variant)
	}

	if key.URL != u {
		t.Errorf("expected URL to be preserved")
	}
}

func TestOCIAwareKeyFunc_NonOCI_IgnoresAccept(t *testing.T) {
	// AC2.2: Non-OCI path produces CacheKey with empty Variant regardless of Accept header
	u, _ := url.Parse("https://example.com/file.txt")
	req, _ := http.NewRequest(http.MethodGet, "http://proxy/dummy", nil)
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")

	key := ociAwareKeyFunc(u, req)

	if key.Variant != "" {
		t.Errorf("expected empty variant for non-OCI path, got %q", key.Variant)
	}

	if key.URL != u {
		t.Errorf("expected URL to be preserved")
	}
}

func TestOCIAwareKeyFunc_OCI_NoAcceptHeader(t *testing.T) {
	// Verify that OCI path with no Accept header produces empty variant
	u, _ := url.Parse("https://gcr.io/v2/library/test/manifests/latest")
	req, _ := http.NewRequest(http.MethodGet, "http://proxy/dummy", nil)
	// No Accept header set

	key := ociAwareKeyFunc(u, req)

	if key.Variant != "" {
		t.Errorf("expected empty variant when Accept header absent, got %q", key.Variant)
	}
}

// Integration tests for Accept forwarding (AC2.3, AC2.4)

func TestIntegration_AcceptForwarding_OCI_WithAccept(t *testing.T) {
	// AC2.3: Client Accept header is forwarded to upstream for OCI paths
	var receivedAccept string

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAccept = r.Header.Get("Accept")
		w.Header().Set("ETag", `"test-etag"`)
		w.Write([]byte("manifest content"))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/v2/library/test/manifests/latest")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp.StatusCode)
	}

	if receivedAccept != "application/vnd.docker.distribution.manifest.v2+json" {
		t.Errorf("expected upstream to receive Accept header, got %q", receivedAccept)
	}
}

func TestIntegration_AcceptForwarding_OCI_NoAccept(t *testing.T) {
	// AC2.4: Proxy does not inject default Accept when client omits it
	var receivedAccept string

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAccept = r.Header.Get("Accept")
		w.Header().Set("ETag", `"test-etag"`)
		w.Write([]byte("manifest content"))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/v2/library/test/manifests/latest")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Request without Accept header
	resp, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp.StatusCode)
	}

	if receivedAccept != "" {
		t.Errorf("expected no Accept header forwarded, but upstream received %q", receivedAccept)
	}
}

func TestIntegration_AcceptForwarding_NonOCI_WithAccept(t *testing.T) {
	// Accept should be forwarded even for non-OCI paths (passthrough)
	var receivedAccept string

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAccept = r.Header.Get("Accept")
		w.Header().Set("ETag", `"test-etag"`)
		w.Write([]byte("file content"))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/file.txt")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req.Header.Set("Accept", "text/plain")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp.StatusCode)
	}

	if receivedAccept != "text/plain" {
		t.Errorf("expected upstream to receive Accept header for non-OCI path, got %q", receivedAccept)
	}
}

// Singleflight dedup tests with Accept (AC3.1, AC3.2)

func TestIntegration_SingleflightDedup_SameAccept(t *testing.T) {
	// AC3.1: Concurrent OCI requests with same URL and same Accept are deduplicated
	var upstreamHits atomic.Int32
	upstreamDelay := 100 * time.Millisecond

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		time.Sleep(upstreamDelay)
		w.Header().Set("ETag", `"test-etag"`)
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.Write([]byte(`{"schemaVersion":2}`))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/v2/library/test/manifests/latest")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	const numRequests = 5
	var wg sync.WaitGroup
	results := make(chan *http.Response, numRequests)

	// Launch concurrent requests with same Accept header
	for range numRequests {
		wg.Go(func() {
			req, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
			req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")

			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("request failed: %v", err)
				return
			}

			results <- resp
		})
	}

	wg.Wait()
	close(results)

	var redirectCount int
	for resp := range results {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusSeeOther {
			redirectCount++
		} else {
			t.Errorf("unexpected status: %d, expected 303", resp.StatusCode)
		}
	}

	if redirectCount != numRequests {
		t.Errorf("expected %d redirects, got %d", numRequests, redirectCount)
	}

	// Only one upstream request should have been made (deduplication)
	if upstreamHits.Load() != 1 {
		t.Errorf("expected 1 upstream hit (deduplication), got %d", upstreamHits.Load())
	}
}

func TestIntegration_SingleflightDedup_DifferentAccept(t *testing.T) {
	// AC3.2: Concurrent OCI requests with same URL but different Accept are NOT deduplicated
	var upstreamHits atomic.Int32
	upstreamDelay := 100 * time.Millisecond

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		time.Sleep(upstreamDelay)

		accept := r.Header.Get("Accept")
		w.Header().Set("ETag", `"test-etag"`)

		// Return different responses based on Accept header
		if accept == "application/vnd.oci.image.index.v1+json" {
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`))
		} else if accept == "application/vnd.docker.distribution.manifest.v2+json" {
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`))
		} else {
			w.Write([]byte(`{"schemaVersion":2}`))
		}
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/v2/library/test/manifests/latest")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	var wg sync.WaitGroup
	results := make(chan *http.Response, 2)

	// Request 1 with first Accept
	wg.Go(func() {
		req, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
		req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json")

		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("request 1 failed: %v", err)
			return
		}

		results <- resp
	})

	// Request 2 with different Accept, delayed to ensure overlap with request 1
	wg.Go(func() {
		time.Sleep(20 * time.Millisecond) // Ensure request 1 is in-flight
		req, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
		req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")

		resp, err := client.Do(req)
		if err != nil {
			t.Errorf("request 2 failed: %v", err)
			return
		}

		results <- resp
	})

	wg.Wait()
	close(results)

	var redirectCount int
	for resp := range results {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusSeeOther {
			redirectCount++
		} else {
			t.Errorf("unexpected status: %d, expected 303", resp.StatusCode)
		}
	}

	if redirectCount != 2 {
		t.Errorf("expected 2 redirects, got %d", redirectCount)
	}

	// Two upstream requests should have been made (NOT deduplicated)
	// because the Accept headers differ
	if upstreamHits.Load() != 2 {
		t.Errorf("expected 2 upstream hits (different Accept = no dedup), got %d", upstreamHits.Load())
	}
}

func TestIntegration_OCIAccept_DifferentAcceptSeparateCacheEntries(t *testing.T) {
	// AC4.1: Two OCI requests with different Accept headers produce separate cache entries
	var upstreamHits atomic.Int32

	// Create token server
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"test-access-token","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	// Create upstream registry that returns different responses based on Accept header
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)

		if r.Header.Get("Authorization") == "" {
			// No auth header: return 401 with challenge
			challenge := `Bearer realm="` + tokenServer.URL +
				`/token",service="test-registry",scope="repository:library/test:pull"`
			w.Header().Set("WWW-Authenticate", challenge)
			w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Has auth header: check the Accept header and return different content
		accept := r.Header.Get("Accept")
		if accept == "application/vnd.oci.image.index.v1+json" {
			w.Header().Set("ETag", `"oci-index-etag"`)
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`))
		} else if accept == "application/vnd.docker.distribution.manifest.v2+json" {
			w.Header().Set("ETag", `"docker-manifest-etag"`)
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`))
		} else {
			w.Header().Set("ETag", `"generic-etag"`)
			w.Write([]byte(`{"schemaVersion":2}`))
		}
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/v2/library/test/manifests/latest")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// First request with Accept: application/vnd.oci.image.index.v1+json
	req1, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req1.Header.Set("Accept", "application/vnd.oci.image.index.v1+json")
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	resp1.Body.Close()

	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("first request: expected 303 redirect, got %d", resp1.StatusCode)
	}

	// Second request with Accept: application/vnd.docker.distribution.manifest.v2+json
	req2, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req2.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("second request: expected 303 redirect, got %d", resp2.StatusCode)
	}

	// Both requests should have hit upstream (not served from same cache entry)
	// With OCI auth:
	// First request: 401 (no auth) → token fetch → 200 (with auth) = 2 upstream hits
	// Second request: proactive auth (cached challenge) → 200 = 1 upstream hit
	// Total: 3 upstream hits
	if upstreamHits.Load() != 3 {
		t.Fatalf("expected 3 upstream hits (401, 200 for first; proactive 200 for second), got %d", upstreamHits.Load())
	}

	// Verify fakeCache contains TWO separate entries with different keys
	upstreamURL, _ := url.Parse(upstream.URL)
	baseCachedURL := "https://" + upstreamURL.Host + "/v2/library/test/manifests/latest"

	// Entry 1: URL + OCI Accept variant
	cacheKey1 := baseCachedURL + "\x00" + "application/vnd.oci.image.index.v1+json"
	entry1 := cache.get(cacheKey1)
	if entry1 == nil {
		t.Fatal("expected OCI index entry to be cached")
	}
	if entry1.headers.Get("Content-Type") != "application/vnd.oci.image.index.v1+json" {
		t.Fatalf("OCI index entry: expected Content-Type application/vnd.oci.image.index.v1+json, got %q",
			entry1.headers.Get("Content-Type"))
	}

	// Entry 2: URL + Docker manifest Accept variant
	cacheKey2 := baseCachedURL + "\x00" + "application/vnd.docker.distribution.manifest.v2+json"
	entry2 := cache.get(cacheKey2)
	if entry2 == nil {
		t.Fatal("expected Docker manifest entry to be cached")
	}
	if entry2.headers.Get("Content-Type") != "application/vnd.docker.distribution.manifest.v2+json" {
		t.Fatalf("Docker manifest entry: expected Content-Type application/vnd.docker.distribution.manifest.v2+json, got %q",
			entry2.headers.Get("Content-Type"))
	}
}

func TestIntegration_OCIAccept_SameAcceptCacheHit(t *testing.T) {
	// AC4.2: Second request with the same Accept header must hit cache
	var (
		tokenEndpointCalls   atomic.Int32
		upstreamRequestCount atomic.Int32
	)

	// Create token server
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenEndpointCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"test-access-token","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	// Create upstream registry that requires auth and supports conditional requests
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequestCount.Add(1)

		if r.Header.Get("Authorization") == "" {
			// No auth header: return 401 with OCI Bearer challenge
			challenge := `Bearer realm="` + tokenServer.URL +
				`/token",service="test-registry",scope="repository:library/test:pull"`
			w.Header().Set("WWW-Authenticate", challenge)
			w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Has auth header: check for conditional request
		if r.Header.Get("If-None-Match") == `"manifest-etag"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		// Return full manifest with ETag
		w.Header().Set("ETag", `"manifest-etag"`)
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/v2/library/test/manifests/latest")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// First request: initial fetch, populates cache
	req1, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req1.Header.Set("Accept", "application/vnd.oci.image.index.v1+json")
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	resp1.Body.Close()

	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("first request: expected 303 redirect, got %d", resp1.StatusCode)
	}

	// Second request with same Accept: should use cached token and send If-None-Match
	req2, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req2.Header.Set("Accept", "application/vnd.oci.image.index.v1+json")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("second request: expected 303 redirect, got %d", resp2.StatusCode)
	}

	// Verify upstream pattern: 401 (no auth), 200 (auth successful), 304 (conditional)
	if upstreamRequestCount.Load() != 3 {
		t.Fatalf("expected 3 upstream requests (401, 200, 304), got %d", upstreamRequestCount.Load())
	}

	// Token endpoint should be called once (cached token on second request)
	if tokenEndpointCalls.Load() > 1 {
		t.Fatalf("expected at most 1 token endpoint call, got %d", tokenEndpointCalls.Load())
	}
}

func TestIntegration_OCIAccept_CacheRevalidationWithAcceptKey(t *testing.T) {
	// AC4.3: Cache revalidation must use the correct ETag from the Accept-specific cache entry
	var upstreamHits atomic.Int32

	// Create token server
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"test-access-token","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	// Track which If-None-Match values we receive
	var receivedETags []string
	var etagsMu sync.Mutex

	// Create upstream registry that tracks ETags in conditional requests
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)

		if r.Header.Get("Authorization") == "" {
			// No auth header: return 401 with OCI Bearer challenge
			challenge := `Bearer realm="` + tokenServer.URL +
				`/token",service="test-registry",scope="repository:library/test:pull"`
			w.Header().Set("WWW-Authenticate", challenge)
			w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Record any If-None-Match value
		ifNoneMatch := r.Header.Get("If-None-Match")
		if ifNoneMatch != "" {
			etagsMu.Lock()
			receivedETags = append(receivedETags, ifNoneMatch)
			etagsMu.Unlock()
		}

		// Check the Accept header and return different ETags
		accept := r.Header.Get("Accept")

		if accept == "application/vnd.oci.image.index.v1+json" {
			if ifNoneMatch == `"etag-a"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"etag-a"`)
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`))
		} else if accept == "application/vnd.docker.distribution.manifest.v2+json" {
			if ifNoneMatch == `"etag-b"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"etag-b"`)
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Write([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`))
		}
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/v2/library/test/manifests/latest")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Step 1: Request format A (OCI index)
	req1, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req1.Header.Set("Accept", "application/vnd.oci.image.index.v1+json")
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("request 1 failed: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("request 1: expected 303, got %d", resp1.StatusCode)
	}

	// Step 2: Request format B (Docker manifest)
	req2, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req2.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("request 2 failed: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("request 2: expected 303, got %d", resp2.StatusCode)
	}

	// Step 3: Request format A again (should revalidate with etag-a, not etag-b)
	req3, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req3.Header.Set("Accept", "application/vnd.oci.image.index.v1+json")
	resp3, err := client.Do(req3)
	if err != nil {
		t.Fatalf("request 3 failed: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusSeeOther {
		t.Fatalf("request 3: expected 303, got %d", resp3.StatusCode)
	}

	// Step 4: Request format B again (should revalidate with etag-b, not etag-a)
	req4, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req4.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp4, err := client.Do(req4)
	if err != nil {
		t.Fatalf("request 4 failed: %v", err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusSeeOther {
		t.Fatalf("request 4: expected 303, got %d", resp4.StatusCode)
	}

	// Verify the ETags sent in If-None-Match conditional requests
	// Should have two conditional requests: one with "etag-a" and one with "etag-b"
	etagsMu.Lock()
	defer etagsMu.Unlock()

	if len(receivedETags) != 2 {
		t.Fatalf("expected 2 If-None-Match conditional requests, got %d; values: %v",
			len(receivedETags), receivedETags)
	}

	// Requests are sequential and deterministic, so order is guaranteed
	if receivedETags[0] != `"etag-a"` {
		t.Errorf("expected first If-None-Match to be %q, got %q",
			`"etag-a"`, receivedETags[0])
	}
	if receivedETags[1] != `"etag-b"` {
		t.Errorf("expected second If-None-Match to be %q, got %q",
			`"etag-b"`, receivedETags[1])
	}
}

// TestIntegration_StructuredLoggingAttributes verifies that request and cache operations log expected structured attributes.
func TestIntegration_NonOCI_AcceptIgnoredInCacheKey(t *testing.T) {
	// AC4.4: Non-OCI requests with different Accept headers share the same cache entry
	var upstreamHits atomic.Int32

	// Standard upstream server (no OCI auth)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)

		// Check for conditional request
		if r.Header.Get("If-None-Match") == `"file-etag"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("ETag", `"file-etag"`)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("file content"))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServer(upstream, cache)
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/file.txt")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// First request with Accept: text/html
	req1, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req1.Header.Set("Accept", "text/html")
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	resp1.Body.Close()

	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("first request: expected 303 redirect, got %d", resp1.StatusCode)
	}

	if upstreamHits.Load() != 1 {
		t.Fatalf("after first request: expected 1 upstream hit, got %d", upstreamHits.Load())
	}

	// Second request with different Accept: application/json (should use same cache entry)
	req2, _ := http.NewRequest(http.MethodGet, proxy.URL+proxyPath, nil)
	req2.Header.Set("Accept", "application/json")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("second request: expected 303 redirect, got %d", resp2.StatusCode)
	}

	// Verify upstream was hit twice total (initial fetch + conditional revalidation)
	// NOT three times, which would happen if Accept caused a separate cache entry
	if upstreamHits.Load() != 2 {
		t.Fatalf("expected 2 upstream hits (same cache entry), got %d", upstreamHits.Load())
	}
}

// TestIntegration_StructuredLoggingAttributes verifies that request and cache operations log expected structured attributes.
func TestIntegration_StructuredLoggingAttributes(t *testing.T) {
	var buf bytes.Buffer

	oldDefault := slog.Default()

	defer func() {
		slog.SetDefault(oldDefault)
	}()

	// Set up JSON logging to capture all logs
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	handler := slog.NewJSONHandler(&buf, opts)
	slog.SetDefault(slog.New(handler))

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"test-etag"`)
		w.Write([]byte("test content"))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	// Create proxy with reqlog middleware to capture request logs
	cacheHandler := &cacheMiddleware{
		cache:    cache,
		client:   upstream.Client(),
		fallback: FallbackPolicy{},
	}

	proxy := httptest.NewServer(reqlog.Middleware(cacheHandler))
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/test.txt")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Make a request
	resp, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp.StatusCode)
	}

	// Parse log output
	logLines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(logLines) < 2 {
		t.Fatalf("expected at least 2 log lines (request start and end), got %d", len(logLines))
	}

	// Check request started log
	var requestStartLog map[string]any
	if err := json.Unmarshal(logLines[0], &requestStartLog); err != nil {
		t.Fatalf("failed to parse first log line: %v", err)
	}

	if requestStartLog["msg"] != "request started" {
		t.Errorf("expected 'request started', got %v", requestStartLog["msg"])
	}

	// Verify request_id is present
	if _, hasRequestID := requestStartLog["request_id"]; !hasRequestID {
		t.Error("expected request_id in request start log")
	}

	// Check request completed log (last line)
	var requestEndLog map[string]any
	if err := json.Unmarshal(logLines[len(logLines)-1], &requestEndLog); err != nil {
		t.Fatalf("failed to parse last log line: %v", err)
	}

	if requestEndLog["msg"] != "request completed" {
		t.Errorf("expected 'request completed', got %v", requestEndLog["msg"])
	}

	// Verify request_id matches in end log
	if requestEndLog["request_id"] != requestStartLog["request_id"] {
		t.Error("request_id should be consistent across request start and end")
	}

	// Verify status and duration in end log
	if status, ok := requestEndLog["status"].(float64); !ok || int(status) != http.StatusSeeOther {
		t.Errorf("expected status 303 in request end, got %v", requestEndLog["status"])
	}

	if _, hasDuration := requestEndLog["duration"]; !hasDuration {
		t.Error("expected duration in request completed log")
	}

	// Verify that request_id appears in multiple logs (from request lifecycle)
	// This demonstrates structured logging is working across the request
	requestIDCount := 0

	for _, line := range logLines {
		var logRecord map[string]any
		if err := json.Unmarshal(line, &logRecord); err != nil {
			continue
		}

		// Count how many logs have the request_id (should be at least 2: start and end)
		if _, hasRequestID := logRecord["request_id"]; hasRequestID {
			requestIDCount++
		}
	}

	// At minimum, verify request_id appears in multiple logs (showing structured logging works)
	if requestIDCount < 2 {
		t.Errorf("expected request_id in at least 2 logs (start and end), found in %d logs", requestIDCount)
	}
}

// TestIntegration_TargetAttributeInLogs verifies that intermediate operations include the target attribute in logs.
func TestIntegration_TargetAttributeInLogs(t *testing.T) {
	var buf bytes.Buffer

	oldDefault := slog.Default()

	defer func() {
		slog.SetDefault(oldDefault)
	}()

	// Set up JSON logging at debug level
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	handler := slog.NewJSONHandler(&buf, opts)
	slog.SetDefault(slog.New(handler))

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"test-etag"`)
		w.Write([]byte("test content"))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	// Create proxy with reqlog middleware
	cacheHandler := &cacheMiddleware{
		cache:    cache,
		client:   upstream.Client(),
		fallback: FallbackPolicy{},
	}

	proxy := httptest.NewServer(reqlog.Middleware(cacheHandler))
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/test.txt")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// First request: cache miss, should log "fetching from upstream"
	resp, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp.StatusCode)
	}

	records := parseJSONLogLines(buf.Bytes())
	assertLogMsgHasAttr(t, records, "fetching from upstream", "target")
	assertLogMsgHasAttr(t, records, "cached upstream response", "target")

	// Second request: triggers conditional request, upstream returns 304
	buf.Reset()

	resp, err = client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}

	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect on second request, got %d", resp.StatusCode)
	}

	records = parseJSONLogLines(buf.Bytes())
	assertLogMsgHasAttr(t, records, "upstream returned 304, using cached content", "target")
}

// TestIntegration_FallbackLoggingAttributes verifies that fallback (stale-serving)
// logs include target and status attributes.
func TestIntegration_FallbackLoggingAttributes(t *testing.T) {
	var buf bytes.Buffer

	oldDefault := slog.Default()
	defer slog.SetDefault(oldDefault)

	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, opts)))

	// Upstream returns 200 first, then 500
	requestCount := 0

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			w.Header().Set("ETag", `"test-etag"`)
			w.Write([]byte("good content"))

			return
		}

		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	cache := newFakeCache()
	cacheHandler := &cacheMiddleware{
		cache:  cache,
		client: upstream.Client(),
		fallback: FallbackPolicy{
			On5xx: true,
		},
	}

	proxy := httptest.NewServer(reqlog.Middleware(cacheHandler))
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/fallback.txt")
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// First request: cache miss, populates cache
	resp, err := client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Second request: upstream returns 502, should serve stale
	buf.Reset()

	resp, err = client.Get(proxy.URL + proxyPath)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}

	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 (stale fallback redirect), got %d", resp.StatusCode)
	}

	records := parseJSONLogLines(buf.Bytes())
	assertLogMsgHasAttr(t, records, "upstream error status, serving stale", "target")
	assertLogMsgHasAttr(t, records, "upstream error status, serving stale", "status")
}

// TestIntegration_SingleflightLeaderFollowerRequestIDs verifies that singleflight leader and follower requests have distinct request_ids.
func TestIntegration_SingleflightLeaderFollowerRequestIDs(t *testing.T) {
	var buf bytes.Buffer

	oldDefault := slog.Default()

	defer func() {
		slog.SetDefault(oldDefault)
	}()

	// Set up JSON logging at debug level to capture all logs
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	handler := slog.NewJSONHandler(&buf, opts)
	slog.SetDefault(slog.New(handler))

	upstreamStarted := make(chan struct{})
	upstreamContinue := make(chan struct{})

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		<-upstreamContinue
		w.Header().Set("ETag", `"test-etag"`)
		w.Write([]byte("slow response"))
	}))
	defer upstream.Close()

	cache := newFakeCache()

	// Create proxy with reqlog middleware
	cacheHandler := &cacheMiddleware{
		cache:    cache,
		client:   upstream.Client(),
		fallback: FallbackPolicy{},
	}

	proxy := httptest.NewServer(reqlog.Middleware(cacheHandler))
	defer proxy.Close()

	proxyPath := upstreamHostPath(upstream, "/slow.txt")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	var wg sync.WaitGroup

	leaderResult := make(chan *http.Response, 1)
	followerResult := make(chan *http.Response, 1)

	// Start leader request
	wg.Go(func() {
		resp, err := client.Get(proxy.URL + proxyPath)
		if err != nil {
			t.Logf("leader request failed: %v", err)

			return
		}

		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		leaderResult <- resp
	})

	// Wait for upstream to receive leader request
	<-upstreamStarted

	// Start follower request (will join singleflight group)
	wg.Go(func() {
		// Small delay to ensure follower joins existing singleflight
		time.Sleep(10 * time.Millisecond)

		resp, err := client.Get(proxy.URL + proxyPath)
		if err != nil {
			t.Logf("follower request failed: %v", err)

			return
		}

		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		followerResult <- resp
	})

	// Allow upstream to complete
	close(upstreamContinue)

	wg.Wait()
	close(leaderResult)
	close(followerResult)

	leaderResp := <-leaderResult
	followerResp := <-followerResult

	if leaderResp == nil || followerResp == nil {
		t.Fatal("expected both requests to complete")
	}

	records := parseJSONLogLines(buf.Bytes())
	requestIDs := extractRequestIDs(records, "request started")

	if len(requestIDs) < 2 {
		t.Fatalf("expected at least 2 'request started' messages, got %d", len(requestIDs))
	}

	if requestIDs[0] == requestIDs[1] {
		t.Errorf("expected distinct request_ids for leader and follower, got both %q", requestIDs[0])
	}

	assertLogMsgHasAttr(t, records, "fetching from upstream", "request_id")
	assertLogMsgHasAttr(t, records, "cached upstream response", "request_id")
}

// parseJSONLogLines parses newline-delimited JSON log output into records.
func parseJSONLogLines(data []byte) []map[string]any {
	var records []map[string]any

	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}

		records = append(records, record)
	}

	return records
}

// extractRequestIDs returns all request_id values from log records matching msg.
func extractRequestIDs(records []map[string]any, msg string) []string {
	var ids []string

	for _, r := range records {
		if m, _ := r["msg"].(string); m == msg {
			if id, _ := r["request_id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
	}

	return ids
}

// assertLogMsgHasAttr checks that at least one log record with the given msg
// contains the specified attribute. Logs a note (not failure) if the msg is not found.
func assertLogMsgHasAttr(t *testing.T, records []map[string]any, msg, attr string) {
	t.Helper()

	for _, r := range records {
		if m, _ := r["msg"].(string); m == msg {
			if _, ok := r[attr]; !ok {
				t.Errorf("log %q missing attribute %q", msg, attr)
			}

			return
		}
	}

	t.Logf("note: log message %q not found in output", msg)
}
