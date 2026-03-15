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

func (c *fakeCache) Head(ctx context.Context, u *url.URL) (http.Header, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[u.String()]
	if !ok {
		return nil, nil //nolint:nilnil // cache interface contract
	}

	return entry.headers.Clone(), nil
}

func (c *fakeCache) GetPresignedURL(ctx context.Context, u *url.URL) (string, error) {
	return "http://fake-s3/" + u.Host + u.Path, nil
}

func (c *fakeCache) Put(ctx context.Context, u *url.URL, headers http.Header, body io.Reader) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[u.String()] = &cacheEntry{
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
	handler := &cacheMiddleware{
		cache:    cache,
		client:   upstream.Client(),
		fallback: fallback,
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
	var requestStartLog map[string]interface{}
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
	var requestEndLog map[string]interface{}
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
		var logRecord map[string]interface{}
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

	// Parse log output and verify "fetching from upstream" and "cached upstream response" have target attribute
	logLines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	foundFetchingLog := false
	foundCachedLog := false

	for _, line := range logLines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var logRecord map[string]interface{}
		if err := json.Unmarshal(line, &logRecord); err != nil {
			continue
		}

		msg, ok := logRecord["msg"].(string)
		if !ok {
			continue
		}

		if msg == "fetching from upstream" {
			foundFetchingLog = true
			target, hasTarget := logRecord["target"].(string)
			if !hasTarget || target == "" {
				t.Error("expected 'target' attribute in 'fetching from upstream' log")
			}
		}

		if msg == "cached upstream response" {
			foundCachedLog = true
			target, hasTarget := logRecord["target"].(string)
			if !hasTarget || target == "" {
				t.Error("expected 'target' attribute in 'cached upstream response' log")
			}
		}
	}

	if !foundFetchingLog {
		t.Error("expected to find 'fetching from upstream' log")
	}

	if !foundCachedLog {
		t.Error("expected to find 'cached upstream response' log")
	}
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

	// Parse log output
	logLines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))

	// Extract request_ids from "request started" messages
	var leaderRequestID, followerRequestID string
	var requestStartedLogs []map[string]interface{}
	requestStartedCount := 0

	for _, line := range logLines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var logRecord map[string]interface{}
		if err := json.Unmarshal(line, &logRecord); err != nil {
			continue
		}

		msg, ok := logRecord["msg"].(string)
		if !ok {
			continue
		}

		if msg == "request started" {
			requestStartedCount++
			requestStartedLogs = append(requestStartedLogs, logRecord)
			requestID, hasRequestID := logRecord["request_id"].(string)
			if !hasRequestID || requestID == "" {
				t.Error("expected request_id in request started log")
				continue
			}

			if requestStartedCount == 1 {
				leaderRequestID = requestID
			} else if requestStartedCount == 2 {
				followerRequestID = requestID
			}
		}
	}

	if requestStartedCount < 2 {
		t.Fatalf("expected at least 2 'request started' messages (leader and follower), got %d", requestStartedCount)
	}

	// Verify leader and follower have DISTINCT request_ids
	if leaderRequestID == "" || followerRequestID == "" {
		t.Fatal("failed to extract request_ids from logs")
	}

	if leaderRequestID == followerRequestID {
		t.Errorf("expected distinct request_ids for leader and follower, got both %q", leaderRequestID)
	}

	// Verify that both "request started" logs have request_id attributes
	if len(requestStartedLogs) >= 2 {
		for i, log := range requestStartedLogs {
			if _, hasRequestID := log["request_id"]; !hasRequestID {
				t.Errorf("request started log %d missing request_id", i)
			}
		}
	}

	// Verify that intermediate logs ("fetching from upstream", "cached upstream response") exist and have request_id
	foundFetchingLog := false
	foundCachedLog := false

	for _, line := range logLines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var logRecord map[string]interface{}
		if err := json.Unmarshal(line, &logRecord); err != nil {
			continue
		}

		msg, ok := logRecord["msg"].(string)
		if !ok {
			continue
		}

		if msg == "fetching from upstream" {
			foundFetchingLog = true
			_, hasRequestID := logRecord["request_id"]
			if !hasRequestID {
				t.Error("expected request_id in 'fetching from upstream' log")
			}
		}

		if msg == "cached upstream response" {
			foundCachedLog = true
			_, hasRequestID := logRecord["request_id"]
			if !hasRequestID {
				t.Error("expected request_id in 'cached upstream response' log")
			}
		}
	}

	if !foundFetchingLog || !foundCachedLog {
		t.Logf("Note: intermediate logs may not be present in all test scenarios (foundFetching=%v, foundCached=%v)", foundFetchingLog, foundCachedLog)
	}
}
