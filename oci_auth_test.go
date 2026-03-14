package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pgregory.net/rapid"
)

const testService = "registry.example.com"

// TestParseOCIAuthChallenge tests the parseOCIAuthChallenge function with table-driven cases.
func TestParseOCIAuthChallenge(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		headers  map[string][]string
		expected ociAuthChallenge
		ok       bool
	}{
		{
			name:   "standard Docker Hub challenge",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/ubuntu:pull"`},
			},
			expected: ociAuthChallenge{
				Realm:   "https://auth.docker.io/token",
				Service: "registry.docker.io",
				Scope:   "repository:library/ubuntu:pull",
			},
			ok: true,
		},
		{
			name:   "unquoted parameter values",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`Bearer realm=https://auth.example.com/token,service=registry.example.com,scope=repository:test:pull`},
			},
			expected: ociAuthChallenge{
				Realm:   "https://auth.example.com/token",
				Service: testService,
				Scope:   "repository:test:pull",
			},
			ok: true,
		},
		{
			name:   "case-insensitive scheme (lowercase bearer)",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`bearer realm="https://auth.example.com/token",service="registry.example.com",scope="repository:test:pull"`},
			},
			expected: ociAuthChallenge{
				Realm:   "https://auth.example.com/token",
				Service: testService,
				Scope:   "repository:test:pull",
			},
			ok: true,
		},
		{
			name:   "case-insensitive parameter keys",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`Bearer Realm="https://auth.example.com/token",Service="registry.example.com",Scope="repository:test:pull"`},
			},
			expected: ociAuthChallenge{
				Realm:   "https://auth.example.com/token",
				Service: testService,
				Scope:   "repository:test:pull",
			},
			ok: true,
		},
		{
			name:   "quoted values with slashes",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`Bearer realm="https://auth.example.com/token",service="registry.example.com",scope="repository:org/repo:pull"`},
			},
			expected: ociAuthChallenge{
				Realm:   "https://auth.example.com/token",
				Service: testService,
				Scope:   "repository:org/repo:pull",
			},
			ok: true,
		},
		{
			name:   "quoted values with escaped quotes",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`Bearer realm="https://auth.example.com/token",service="registry.example.com",scope="repo:\"name\":pull"`},
			},
			expected: ociAuthChallenge{
				Realm:   "https://auth.example.com/token",
				Service: testService,
				Scope:   `repo:"name":pull`,
			},
			ok: true,
		},
		{
			name:   "multiple WWW-Authenticate headers (Bearer + Basic), pick first Bearer",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {
					`Basic realm="registry"`,
					`Bearer realm="https://auth.example.com/token",service="registry.example.com",scope="repository:test:pull"`,
				},
			},
			expected: ociAuthChallenge{
				Realm:   "https://auth.example.com/token",
				Service: testService,
				Scope:   "repository:test:pull",
			},
			ok: true,
		},
		{
			name:   "extra whitespace around = and after commas",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {
					`Bearer realm = "https://auth.example.com/token" , service = "registry.example.com" , scope = "repository:test:pull"`,
				},
			},
			expected: ociAuthChallenge{
				Realm:   "https://auth.example.com/token",
				Service: testService,
				Scope:   "repository:test:pull",
			},
			ok: true,
		},
		{
			name:    "401 with no WWW-Authenticate header",
			status:  401,
			headers: map[string][]string{},
			ok:      false,
		},
		{
			name:   "200 with WWW-Authenticate header (not 401)",
			status: 200,
			headers: map[string][]string{
				"Www-Authenticate": {`Bearer realm="https://auth.example.com/token",service="registry.example.com",scope="repository:test:pull"`},
			},
			ok: false,
		},
		{
			name:   "401 with Basic challenge",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`Basic realm="registry"`},
			},
			ok: false,
		},
		{
			name:   "401 with Digest challenge",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`Digest realm="registry",qop="auth"`},
			},
			ok: false,
		},
		{
			name:   "Bearer with realm and service but no scope",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`Bearer realm="https://auth.example.com/token",service="registry.example.com"`},
			},
			ok: false,
		},
		{
			name:   "Bearer with realm and scope but no service",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`Bearer realm="https://auth.example.com/token",scope="repository:test:pull"`},
			},
			ok: false,
		},
		{
			name:   "Bearer with only realm",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`Bearer realm="https://auth.example.com/token"`},
			},
			ok: false,
		},
		{
			name:   "whitespace before Bearer scheme",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`  Bearer realm="https://auth.example.com/token",service="registry.example.com",scope="repository:test:pull"`},
			},
			expected: ociAuthChallenge{
				Realm:   "https://auth.example.com/token",
				Service: testService,
				Scope:   "repository:test:pull",
			},
			ok: true,
		},
		{
			name:   "401 with BearerToken scheme (not Bearer) — rejects false positive",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`BearerToken realm="https://auth.example.com/token",service="registry.example.com",scope="repository:test:pull"`},
			},
			ok: false,
		},
		{
			name:   "401 with Bearerx scheme (not Bearer) — rejects false positive",
			status: 401,
			headers: map[string][]string{
				"Www-Authenticate": {`Bearerx realm="https://auth.example.com/token",service="registry.example.com",scope="repository:test:pull"`},
			},
			ok: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: tt.status,
				Header:     http.Header{},
			}

			for key, values := range tt.headers {
				for _, value := range values {
					resp.Header.Add(key, value)
				}
			}

			challenge, ok := parseOCIAuthChallenge(resp)

			if ok != tt.ok {
				t.Errorf("parseOCIAuthChallenge returned ok=%v, want %v", ok, tt.ok)
			}

			if ok && challenge != tt.expected {
				t.Errorf("parseOCIAuthChallenge returned %+v, want %+v", challenge, tt.expected)
			}
		})
	}
}

// TestParseOCIAuthChallenge_Property tests the round-trip property:
// given random valid (realm, service, scope) triples, construct a WWW-Authenticate header,
// parse it, and verify correctness.
func TestParseOCIAuthChallenge_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		realm := rapid.StringMatching(`[a-z0-9:/._-]+`).Draw(t, "realm")
		service := rapid.StringMatching(`[a-z0-9._:-]+`).Draw(t, "service")
		scope := rapid.StringMatching(`[a-z0-9:/_-]+`).Draw(t, "scope")

		// Construct a WWW-Authenticate header
		headerValue := `Bearer realm="` + realm + `",service="` + service + `",scope="` + scope + `"`

		resp := &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header: http.Header{
				"Www-Authenticate": {headerValue},
			},
		}

		challenge, ok := parseOCIAuthChallenge(resp)

		if !ok {
			t.Fatalf("parseOCIAuthChallenge failed to parse valid header: %s", headerValue)
		}

		if challenge.Realm != realm {
			t.Errorf("Realm mismatch: got %q, want %q", challenge.Realm, realm)
		}

		if challenge.Service != service {
			t.Errorf("Service mismatch: got %q, want %q", challenge.Service, service)
		}

		if challenge.Scope != scope {
			t.Errorf("Scope mismatch: got %q, want %q", challenge.Scope, scope)
		}
	})
}

// TestTokenCache_StoreAndRetrieve tests storage and retrieval by key.
func TestTokenCache_StoreAndRetrieve(t *testing.T) {
	cache := newTokenCache()
	expiry := time.Now().Add(time.Hour)

	c1 := ociAuthChallenge{Realm: "realm1", Service: "service1", Scope: "scope1"}
	c2 := ociAuthChallenge{Realm: "realm2", Service: "service2", Scope: "scope2"}

	// Set and retrieve a token
	cache.set(c1, "token1", expiry)

	token, ok := cache.get(c1)
	if !ok {
		t.Errorf("failed to retrieve token for existing key")
	}

	if token != "token1" {
		t.Errorf("token mismatch: got %q, want %q", token, "token1")
	}

	// Set tokens for two different tuples
	cache.set(c2, "token2", expiry)

	token2, ok2 := cache.get(c2)
	if !ok2 {
		t.Errorf("failed to retrieve token for second key")
	}

	if token2 != "token2" {
		t.Errorf("token mismatch: got %q, want %q", token2, "token2")
	}

	// Verify first token is still there
	token1, ok1 := cache.get(c1)
	if !ok1 || token1 != "token1" {
		t.Errorf("first token was overwritten or lost")
	}

	// Lookup for non-existent key
	_, ok = cache.get(ociAuthChallenge{Realm: "nonexistent", Service: "realm", Scope: "key"})
	if ok {
		t.Errorf("lookup for non-existent key should return false")
	}
}

// TestTokenCache_ExpiredTokenNotReturned tests that expired tokens are not returned.
func TestTokenCache_ExpiredTokenNotReturned(t *testing.T) {
	cache := newTokenCache()

	c1 := ociAuthChallenge{Realm: "realm", Service: "service", Scope: "scope"}
	c2 := ociAuthChallenge{Realm: "realm2", Service: "service2", Scope: "scope2"}

	// Set token with expiry in the past
	pastExpiry := time.Now().Add(-time.Second)
	cache.set(c1, "expiredtoken", pastExpiry)

	token, ok := cache.get(c1)
	if ok {
		t.Errorf("expired token should not be returned, got ok=%v", ok)
	}

	if token != "" {
		t.Errorf("expired token should return empty string, got %q", token)
	}

	// Set token with expiry in the future
	futureExpiry := time.Now().Add(time.Hour)
	cache.set(c2, "futuretoken", futureExpiry)

	token, ok = cache.get(c2)
	if !ok {
		t.Errorf("future token should be returned, got ok=false")
	}

	if token != "futuretoken" {
		t.Errorf("token mismatch: got %q, want %q", token, "futuretoken")
	}
}

// TestTokenCache_ConcurrentAccess tests concurrent safety.
func TestTokenCache_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	cache := newTokenCache()
	expiry := time.Now().Add(time.Hour)

	var wg sync.WaitGroup

	numGoroutines := 100

	for i := range numGoroutines {
		id := i

		wg.Go(func() {
			c := ociAuthChallenge{
				Realm:   "realm" + strconv.Itoa(id%10),
				Service: "service" + strconv.Itoa(id%5),
				Scope:   "scope" + strconv.Itoa(id%3),
			}

			// Alternate between set and get
			if id%2 == 0 {
				cache.set(c, "token"+strconv.Itoa(id), expiry)
			} else {
				cache.get(c)
			}
		})
	}

	wg.Wait()
}

// TestTokenCache_Property_NonExpiredTokenRetrievable is a property test verifying
// that all non-expired tokens set are retrievable with matching values.
func TestTokenCache_Property_NonExpiredTokenRetrievable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cache := newTokenCache()
		futureExpiry := time.Now().Add(time.Hour)

		// Generate random tokens and store them
		realm := rapid.StringMatching(`[a-z0-9:/_.-]+`).Draw(t, "realm")
		service := rapid.StringMatching(`[a-z0-9._:-]+`).Draw(t, "service")
		scope := rapid.StringMatching(`[a-z0-9:/_-]+`).Draw(t, "scope")
		tokenVal := rapid.StringMatching(`[a-zA-Z0-9._-]+`).Draw(t, "token")

		c := ociAuthChallenge{Realm: realm, Service: service, Scope: scope}
		cache.set(c, tokenVal, futureExpiry)

		// Verify we can retrieve it
		retrieved, ok := cache.get(c)
		if !ok {
			t.Fatalf("failed to retrieve token with future expiry for key (%q, %q, %q)",
				realm, service, scope)
		}

		if retrieved != tokenVal {
			t.Errorf("token mismatch: got %q, want %q", retrieved, tokenVal)
		}
	})
}

// TestFetchToken_Success tests successful token fetch with token field.
func TestFetchToken_Success(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify query parameters
		if r.URL.Query().Get("service") != testService {
			t.Errorf("service param mismatch: got %q", r.URL.Query().Get("service"))
		}

		if r.URL.Query().Get("scope") != "repository:test:pull" {
			t.Errorf("scope param mismatch: got %q", r.URL.Query().Get("scope"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"test-token","expires_in":3600}`))
	}))
	defer server.Close()

	transport := server.Client().Transport
	challenge := ociAuthChallenge{
		Realm:   server.URL,
		Service: testService,
		Scope:   "repository:test:pull",
	}

	ctx := testContext(t)

	token, expiry, err := fetchToken(ctx, transport, challenge)
	if err != nil {
		t.Fatalf("fetchToken failed: %v", err)
	}

	if token != "test-token" {
		t.Errorf("token mismatch: got %q, want %q", token, "test-token")
	}

	// Verify expiry is approximately now + 3600s - 30s
	now := time.Now()

	expectedExpiry := now.Add(3570 * time.Second)
	if expiry.Before(expectedExpiry.Add(-time.Second)) || expiry.After(expectedExpiry.Add(time.Second)) {
		t.Errorf("expiry out of expected range: got %v, expected near %v", expiry, expectedExpiry)
	}
}

// TestFetchToken_AccessTokenField tests successful token fetch with access_token field.
func TestFetchToken_AccessTokenField(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"alt-token","expires_in":300}`))
	}))
	defer server.Close()

	transport := server.Client().Transport
	challenge := ociAuthChallenge{
		Realm:   server.URL,
		Service: testService,
		Scope:   "repository:test:pull",
	}

	ctx := testContext(t)

	token, _, err := fetchToken(ctx, transport, challenge)
	if err != nil {
		t.Fatalf("fetchToken failed: %v", err)
	}

	if token != "alt-token" {
		t.Errorf("token mismatch: got %q, want %q", token, "alt-token")
	}
}

// TestFetchToken_DefaultExpiresIn tests default expiry when expires_in is missing.
func TestFetchToken_DefaultExpiresIn(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"short-lived"}`))
	}))
	defer server.Close()

	transport := server.Client().Transport
	challenge := ociAuthChallenge{
		Realm:   server.URL,
		Service: testService,
		Scope:   "repository:test:pull",
	}

	ctx := testContext(t)

	token, expiry, err := fetchToken(ctx, transport, challenge)
	if err != nil {
		t.Fatalf("fetchToken failed: %v", err)
	}

	if token != "short-lived" {
		t.Errorf("token mismatch: got %q, want %q", token, "short-lived")
	}

	// Verify expiry defaults to approximately now + 60s - 30s
	now := time.Now()

	expectedExpiry := now.Add(30 * time.Second)
	if expiry.Before(expectedExpiry.Add(-time.Second)) || expiry.After(expectedExpiry.Add(time.Second)) {
		t.Errorf("expiry out of expected range: got %v, expected near %v", expiry, expectedExpiry)
	}
}

// TestFetchToken_Non200Response tests non-200 response from realm.
func TestFetchToken_Non200Response(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	transport := server.Client().Transport
	challenge := ociAuthChallenge{
		Realm:   server.URL,
		Service: testService,
		Scope:   "repository:test:pull",
	}

	ctx := testContext(t)

	_, _, err := fetchToken(ctx, transport, challenge)
	if err == nil {
		t.Errorf("fetchToken should return error for non-200 response")
	}

	if err != nil && !strings.Contains(err.Error(), "403") && !strings.Contains(err.Error(), "token endpoint returned") {
		t.Errorf("error message should contain '403' or 'token endpoint returned', got: %v", err)
	}
}

// TestFetchToken_NetworkError tests network error during fetch.
func TestFetchToken_NetworkError(t *testing.T) {
	transport := http.DefaultTransport
	challenge := ociAuthChallenge{
		Realm:   "https://127.0.0.1:1",
		Service: testService,
		Scope:   "repository:test:pull",
	}

	ctx := testContext(t)

	_, _, err := fetchToken(ctx, transport, challenge)
	if err == nil {
		t.Errorf("fetchToken should return error for network error")
	}

	if err != nil &&
		!strings.Contains(err.Error(), "connection") &&
		!strings.Contains(err.Error(), "dial") &&
		!strings.Contains(err.Error(), "refused") {
		t.Errorf("error should mention connection/dial/refused, got: %v", err)
	}
}

// TestFetchToken_RejectsNonHTTPS tests that non-HTTPS realm URLs are rejected.
func TestFetchToken_RejectsNonHTTPS(t *testing.T) {
	transport := http.DefaultTransport
	challenge := ociAuthChallenge{
		Realm:   "http://auth.example.com/token",
		Service: testService,
		Scope:   "repository:test:pull",
	}

	ctx := testContext(t)

	_, _, err := fetchToken(ctx, transport, challenge)
	if err == nil {
		t.Fatal("fetchToken should reject non-HTTPS realm URL")
	}

	if !strings.Contains(err.Error(), "not https") {
		t.Errorf("error should mention https requirement, got: %v", err)
	}
}

// TestFetchToken_MalformedJSON tests malformed JSON response.
func TestFetchToken_MalformedJSON(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{invalid json`))
	}))
	defer server.Close()

	transport := server.Client().Transport
	challenge := ociAuthChallenge{
		Realm:   server.URL,
		Service: testService,
		Scope:   "repository:test:pull",
	}

	ctx := testContext(t)

	_, _, err := fetchToken(ctx, transport, challenge)
	if err == nil {
		t.Errorf("fetchToken should return error for malformed JSON")
	}

	if err != nil && !strings.Contains(err.Error(), "decode") && !strings.Contains(err.Error(), "json") {
		t.Errorf("error message should contain decode/json text, got: %v", err)
	}
}

// TestFetchToken_EmptyTokenFields tests response with empty token fields.
func TestFetchToken_EmptyTokenFields(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"expires_in":300}`))
	}))
	defer server.Close()

	transport := server.Client().Transport
	challenge := ociAuthChallenge{
		Realm:   server.URL,
		Service: testService,
		Scope:   "repository:test:pull",
	}

	ctx := testContext(t)

	_, _, err := fetchToken(ctx, transport, challenge)
	if err == nil {
		t.Errorf("fetchToken should return error when both token fields are empty")
	}

	if err != nil && !strings.Contains(err.Error(), "missing") {
		t.Errorf("error message should contain 'missing', got: %v", err)
	}
}

// TestFetchToken_RealmWithExistingQueryParams tests that realm URLs with existing query parameters
// are correctly merged with service and scope parameters.
func TestFetchToken_RealmWithExistingQueryParams(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify all query parameters are present
		q := r.URL.Query()
		if q.Get("client_id") != "foo" {
			t.Errorf("client_id param mismatch: got %q, want %q", q.Get("client_id"), "foo")
		}

		if q.Get("service") != testService {
			t.Errorf("service param mismatch: got %q", q.Get("service"))
		}

		if q.Get("scope") != "repository:test:pull" {
			t.Errorf("scope param mismatch: got %q", q.Get("scope"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"test-token","expires_in":3600}`))
	}))
	defer server.Close()

	transport := server.Client().Transport
	// Realm URL already has query parameters
	realmWithParams := server.URL + "?client_id=foo"
	challenge := ociAuthChallenge{
		Realm:   realmWithParams,
		Service: testService,
		Scope:   "repository:test:pull",
	}

	ctx := testContext(t)

	token, _, err := fetchToken(ctx, transport, challenge)
	if err != nil {
		t.Fatalf("fetchToken failed: %v", err)
	}

	if token != "test-token" {
		t.Errorf("token mismatch: got %q, want %q", token, "test-token")
	}
}

// TestTokenFetcher_SingleflightDedup tests singleflight deduplication.
func TestTokenFetcher_SingleflightDedup(t *testing.T) {
	var requestCount atomic.Int32

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		// Simulate slow token endpoint
		time.Sleep(50 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"shared-token","expires_in":3600}`))
	}))
	defer server.Close()

	cache := newTokenCache()
	fetcher := &tokenFetcher{
		base:  server.Client().Transport,
		cache: cache,
	}

	challenge := ociAuthChallenge{
		Realm:   server.URL,
		Service: testService,
		Scope:   "repository:test:pull",
	}

	// Launch 10 concurrent getOrFetch calls with the same challenge
	var wg sync.WaitGroup

	tokens := make([]string, 10)
	errs := make([]error, 10)

	for i := range 10 {
		id := i

		wg.Go(func() {
			ctx := testContext(t)

			token, err := fetcher.getOrFetch(ctx, challenge)
			tokens[id] = token
			errs[id] = err
		})
	}

	wg.Wait()

	// Verify exactly 1 server request
	if requestCount.Load() != 1 {
		t.Errorf("server received %d requests, want 1", requestCount.Load())
	}

	// Verify all goroutines got the same token
	for i := range 10 {
		if errs[i] != nil {
			t.Errorf("getOrFetch[%d] failed: %v", i, errs[i])
		}

		if tokens[i] != "shared-token" {
			t.Errorf("getOrFetch[%d] returned %q, want %q", i, tokens[i], "shared-token")
		}
	}
}

// testContext returns a context with a 5-second timeout whose cancellation
// is tied to the test's cleanup lifecycle.
func testContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	return ctx
}

// TestExtractOCIRepository tests the extractOCIRepository function with table-driven cases.
func TestExtractOCIRepository(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		expectedHost string
		expectedRepo string
		expectedOK   bool
	}{
		{
			name:         "standard manifests path with multi-level repo",
			path:         "/v2/library/ubuntu/manifests/latest",
			expectedHost: "example.com",
			expectedRepo: "library/ubuntu",
			expectedOK:   true,
		},
		{
			name:         "three-level repository path with blobs",
			path:         "/v2/my-org/my-project/my-image/blobs/sha256:abc",
			expectedHost: "example.com",
			expectedRepo: "my-org/my-project/my-image",
			expectedOK:   true,
		},
		{
			name:         "two-level repository with tags",
			path:         "/v2/library/ubuntu/tags/list",
			expectedHost: "example.com",
			expectedRepo: "library/ubuntu",
			expectedOK:   true,
		},
		{
			name:         "single-level repository with blobs uploads",
			path:         "/v2/repo/blobs/uploads/uuid",
			expectedHost: "example.com",
			expectedRepo: "repo",
			expectedOK:   true,
		},
		{
			name:         "single-level repository with manifests and digest",
			path:         "/v2/single/manifests/sha256:def",
			expectedHost: "example.com",
			expectedRepo: "single",
			expectedOK:   true,
		},
		{
			name:         "v2 with trailing slash only",
			path:         "/v2/",
			expectedHost: "",
			expectedRepo: "",
			expectedOK:   false,
		},
		{
			name:         "non-OCI path",
			path:         "/other/path",
			expectedHost: "",
			expectedRepo: "",
			expectedOK:   false,
		},
		{
			name:         "v2 without trailing slash",
			path:         "/v2",
			expectedHost: "",
			expectedRepo: "",
			expectedOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := &url.URL{
				Host: "example.com",
				Path: tt.path,
			}

			host, repo, ok := extractOCIRepository(u)

			if ok != tt.expectedOK {
				t.Errorf("extractOCIRepository returned ok=%v, want %v", ok, tt.expectedOK)
			}

			if ok {
				if host != tt.expectedHost {
					t.Errorf("extractOCIRepository returned host=%q, want %q", host, tt.expectedHost)
				}

				if repo != tt.expectedRepo {
					t.Errorf("extractOCIRepository returned repo=%q, want %q", repo, tt.expectedRepo)
				}
			}
		})
	}
}

// TestOCIAuthTransport_Discovery tests the discovery path: bare request gets 401 with OCI challenge,
// token is fetched, and resent request succeeds.
func TestOCIAuthTransport_Discovery(t *testing.T) {
	// Token server
	tokenCalls := atomic.Int32{}

	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)

		// Verify query parameters
		if r.URL.Query().Get("service") != testService {
			t.Errorf("token: service param mismatch: got %q", r.URL.Query().Get("service"))
		}

		if r.URL.Query().Get("scope") != "repository:library/test:pull" {
			t.Errorf("token: scope param mismatch: got %q", r.URL.Query().Get("scope"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"test-token-disco","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	// Upstream registry (TLS)
	upstreamCalls := atomic.Int32{}

	upstreamServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls := upstreamCalls.Add(1)

		if calls == 1 {
			// First call: bare request, return 401 with challenge
			w.Header().Set("Www-Authenticate",
				`Bearer realm="`+tokenServer.URL+`",service="registry.example.com",scope="repository:library/test:pull"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		// Second call: should have Authorization header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token-disco" {
			t.Errorf("upstream call 2: expected 'Bearer test-token-disco', got %q", auth)
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("response body"))
	}))
	defer upstreamServer.Close()

	// Create transport
	transport := newOCIAuthTransport(upstreamServer.Client().Transport)
	defer transport.Close()

	// Make request
	ctx := testContext(t)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamServer.URL+"/v2/library/test/manifests/latest", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer resp.Body.Close()

	// Verify final response is 200
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	// Verify upstream received 2 requests
	if upstreamCalls.Load() != 2 {
		t.Errorf("upstream received %d calls, want 2", upstreamCalls.Load())
	}

	// Verify token endpoint received 1 request
	if tokenCalls.Load() != 1 {
		t.Errorf("token endpoint received %d calls, want 1", tokenCalls.Load())
	}
}

// TestOCIAuthTransport_Proactive tests the proactive path: second request to same repository
// uses cached token and makes only one upstream request.
func TestOCIAuthTransport_Proactive(t *testing.T) {
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"test-token-proactive","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	upstreamCalls := atomic.Int32{}

	upstreamServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls := upstreamCalls.Add(1)

		if calls == 1 {
			// First request: bare, return 401
			w.Header().Set("Www-Authenticate",
				`Bearer realm="`+tokenServer.URL+`",service="registry.example.com",scope="repository:library/test:pull"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		// Subsequent requests: should have Authorization header
		auth := r.Header.Get("Authorization")
		if auth == "" {
			t.Errorf("request %d: expected Authorization header", calls)
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstreamServer.Close()

	transport := newOCIAuthTransport(upstreamServer.Client().Transport)
	defer transport.Close()

	// First request: discovery path
	ctx1 := testContext(t)

	req1, _ := http.NewRequestWithContext(ctx1, http.MethodGet, upstreamServer.URL+"/v2/library/test/manifests/latest", nil)

	resp1, err := transport.RoundTrip(req1)
	if err != nil {
		t.Fatalf("first RoundTrip failed: %v", err)
	}

	resp1.Body.Close()

	if resp1.StatusCode != http.StatusOK {
		t.Errorf("first response: expected 200, got %d", resp1.StatusCode)
	}

	// Second request: proactive path (same repository)
	ctx2 := testContext(t)

	req2, _ := http.NewRequestWithContext(ctx2, http.MethodGet, upstreamServer.URL+"/v2/library/test/manifests/another-tag", nil)

	resp2, err := transport.RoundTrip(req2)
	if err != nil {
		t.Fatalf("second RoundTrip failed: %v", err)
	}

	resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Errorf("second response: expected 200, got %d", resp2.StatusCode)
	}

	// First request does discovery: bare + retry = 2 calls
	// Second request uses proactive path: direct proactive = 1 call
	// Total: 3 calls
	if upstreamCalls.Load() != 3 {
		t.Errorf("upstream received %d calls, want 3 (discovery=2, proactive=1)", upstreamCalls.Load())
	}
}

// TestOCIAuthTransport_Refresh tests token refresh: near-expiry token is refreshed.
func TestOCIAuthTransport_Refresh(t *testing.T) {
	tokenCalls := atomic.Int32{}

	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := tokenCalls.Add(1)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if call == 1 {
			// First call: return token with 31 second expiry (1 second useful life after 30s buffer)
			_, _ = w.Write([]byte(`{"token":"test-token-first","expires_in":31}`))
		} else {
			// Second call: return new token
			_, _ = w.Write([]byte(`{"token":"test-token-refreshed","expires_in":3600}`))
		}
	}))
	defer tokenServer.Close()

	upstreamCalls := atomic.Int32{}

	upstreamServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls := upstreamCalls.Add(1)

		if calls == 1 {
			// First request: bare, return 401
			w.Header().Set("Www-Authenticate",
				`Bearer realm="`+tokenServer.URL+`",service="registry.example.com",scope="repository:library/test:pull"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		// Subsequent requests: accept token (any token)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstreamServer.Close()

	transport := newOCIAuthTransport(upstreamServer.Client().Transport)
	defer transport.Close()

	// First request: gets 31-second expiry token
	ctx1 := testContext(t)

	req1, _ := http.NewRequestWithContext(ctx1, http.MethodGet, upstreamServer.URL+"/v2/library/test/manifests/latest", nil)
	resp1, _ := transport.RoundTrip(req1)
	resp1.Body.Close()

	// Wait 2 seconds (token expires in 1 second)
	time.Sleep(2 * time.Second)

	// Second request: should trigger refresh (token expired)
	ctx2 := testContext(t)

	req2, _ := http.NewRequestWithContext(ctx2, http.MethodGet, upstreamServer.URL+"/v2/library/test/manifests/latest", nil)
	resp2, _ := transport.RoundTrip(req2)
	resp2.Body.Close()

	// Token endpoint should have been called twice (original + refresh)
	if tokenCalls.Load() != 2 {
		t.Errorf("token endpoint called %d times, want 2 (original + refresh)", tokenCalls.Load())
	}
}

// TestOCIAuthTransport_ResendStill401 tests that if resend still gets 401, that 401 is returned to caller.
func TestOCIAuthTransport_ResendStill401(t *testing.T) {
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"test-token","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	upstreamCalls := atomic.Int32{}

	upstreamServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always return 401 (token is invalid/revoked)
		upstreamCalls.Add(1)
		w.Header().Set("Www-Authenticate",
			`Bearer realm="`+tokenServer.URL+`",service="registry.example.com",scope="repository:library/test:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstreamServer.Close()

	transport := newOCIAuthTransport(upstreamServer.Client().Transport)
	defer transport.Close()

	ctx := testContext(t)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstreamServer.URL+"/v2/library/test/manifests/latest", nil)

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer resp.Body.Close()

	// Should return 401 to caller
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}

	// Upstream should have received exactly 2 requests (bare + retry with token)
	if upstreamCalls.Load() != 2 {
		t.Errorf("upstream received %d calls, want 2 (bare + retry)", upstreamCalls.Load())
	}
}

// TestOCIAuthTransport_ExistingAuthBypass tests that existing Authorization header bypasses OCI logic.
func TestOCIAuthTransport_ExistingAuthBypass(t *testing.T) {
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("token server should not be called")
	}))
	defer tokenServer.Close()

	upstreamCalls := atomic.Int32{}

	upstreamServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)

		// Verify existing Authorization is preserved
		auth := r.Header.Get("Authorization")
		if auth != "Bearer existing-token" {
			t.Errorf("expected 'Bearer existing-token', got %q", auth)
		}

		// Return 401 anyway (but it should pass through)
		w.Header().Set("Www-Authenticate",
			`Bearer realm="`+tokenServer.URL+`",service="registry.example.com",scope="repository:test:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstreamServer.Close()

	transport := newOCIAuthTransport(upstreamServer.Client().Transport)
	defer transport.Close()

	ctx := testContext(t)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstreamServer.URL+"/v2/library/test/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer existing-token")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer resp.Body.Close()

	// Should return 401 as-is
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}

	// Upstream should have received exactly 1 request (no retry)
	if upstreamCalls.Load() != 1 {
		t.Errorf("upstream received %d calls, want 1", upstreamCalls.Load())
	}
}

// TestOCIAuthTransport_StaleTokenRediscovery tests stale token eviction and re-discovery.
// Upstream initially accepts token A, then rejects it and accepts token B.
func TestOCIAuthTransport_StaleTokenRediscovery(t *testing.T) {
	tokenCalls := atomic.Int32{}

	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := tokenCalls.Add(1)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if call == 1 {
			_, _ = w.Write([]byte(`{"token":"token-a","expires_in":3600}`))
		} else {
			_, _ = w.Write([]byte(`{"token":"token-b","expires_in":3600}`))
		}
	}))
	defer tokenServer.Close()

	upstreamCalls := atomic.Int32{}

	upstreamServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCalls.Add(1)

		if call == 1 {
			// First request: bare, return 401
			w.Header().Set("Www-Authenticate",
				`Bearer realm="`+tokenServer.URL+`",service="registry.example.com",scope="repository:library/test:pull"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		if call == 2 {
			// Second request: should be retry with token-a, accept it
			auth := r.Header.Get("Authorization")
			if auth != "Bearer token-a" {
				t.Errorf("call 2: expected 'Bearer token-a', got %q", auth)
			}

			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))

			return
		}

		if call == 3 {
			// Third request: proactive with token-a, but now reject it (token is stale)
			auth := r.Header.Get("Authorization")
			if auth != "Bearer token-a" {
				t.Errorf("call 3: expected 'Bearer token-a', got %q", auth)
			}
			// Reject with challenge to trigger re-discovery
			w.Header().Set("Www-Authenticate",
				`Bearer realm="`+tokenServer.URL+`",service="registry.example.com",scope="repository:library/test:pull"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		if call == 4 {
			// Fourth request: bare re-discovery, return 401 with challenge
			w.Header().Set("Www-Authenticate",
				`Bearer realm="`+tokenServer.URL+`",service="registry.example.com",scope="repository:library/test:pull"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		if call == 5 {
			// Fifth request: retry with token-b (from fresh discovery), should succeed
			auth := r.Header.Get("Authorization")
			if auth != "Bearer token-b" {
				t.Errorf("call 5: expected 'Bearer token-b', got %q", auth)
			}

			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer upstreamServer.Close()

	transport := newOCIAuthTransport(upstreamServer.Client().Transport)
	defer transport.Close()

	// First request: discovery path with token-a
	ctx1 := testContext(t)

	req1, _ := http.NewRequestWithContext(ctx1, http.MethodGet, upstreamServer.URL+"/v2/library/test/manifests/latest", nil)
	resp1, _ := transport.RoundTrip(req1)
	resp1.Body.Close()

	if resp1.StatusCode != http.StatusOK {
		t.Errorf("first response: expected 200, got %d", resp1.StatusCode)
	}

	// Second request: proactive with token-a (succeeds), then stale token is detected
	// and re-discovery happens with token-b
	ctx2 := testContext(t)

	req2, _ := http.NewRequestWithContext(ctx2, http.MethodGet, upstreamServer.URL+"/v2/library/test/manifests/another-tag", nil)
	resp2, _ := transport.RoundTrip(req2)
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Errorf("second response: expected 200, got %d", resp2.StatusCode)
	}

	// Verify call sequence:
	// 1. bare request
	// 2. retry with token-a (succeeds)
	// 3. proactive with token-a (fails with 401)
	// 4. bare re-discovery
	// 5. retry with token-b (succeeds)
	if upstreamCalls.Load() != 5 {
		t.Errorf("upstream received %d calls, want 5", upstreamCalls.Load())
	}

	// Token server should have been called twice (token-a + token-b)
	if tokenCalls.Load() != 2 {
		t.Errorf("token server called %d times, want 2", tokenCalls.Load())
	}
}

// TestOCIAuthTransport_NonOCI401Passthrough tests that non-OCI 401 passes through.
func TestOCIAuthTransport_NonOCI401Passthrough(t *testing.T) {
	upstreamServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return 401 without OCI challenge (no WWW-Authenticate header)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstreamServer.Close()

	transport := newOCIAuthTransport(upstreamServer.Client().Transport)
	defer transport.Close()

	ctx := testContext(t)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstreamServer.URL+"/v2/library/test/manifests/latest", nil)

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer resp.Body.Close()

	// Should return 401 as-is
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// TestOCIAuthTransport_TokenFetchFailure tests that when token fetch fails, original 401 is returned.
func TestOCIAuthTransport_TokenFetchFailure(t *testing.T) {
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Token server fails
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer tokenServer.Close()

	upstreamServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate",
			`Bearer realm="`+tokenServer.URL+`",service="registry.example.com",scope="repository:test:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstreamServer.Close()

	transport := newOCIAuthTransport(upstreamServer.Client().Transport)
	defer transport.Close()

	ctx := testContext(t)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstreamServer.URL+"/v2/library/test/manifests/latest", nil)

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer resp.Body.Close()

	// Should return 401 (from upstream, not an error)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// TestOCIAuthTransport_NonGETMethod tests that non-idempotent methods don't retry.
func TestOCIAuthTransport_NonGETMethod(t *testing.T) {
	upstreamCalls := atomic.Int32{}

	upstreamServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)

		w.Header().Set("Www-Authenticate",
			`Bearer realm="https://localhost:8080/token",service="registry.example.com",scope="repository:test:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstreamServer.Close()

	transport := newOCIAuthTransport(upstreamServer.Client().Transport)
	defer transport.Close()

	ctx := testContext(t)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstreamServer.URL+"/v2/library/test/blobs/uploads/", nil)

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer resp.Body.Close()

	// Should return 401 as-is
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}

	// Should have received only 1 request (no retry for POST)
	if upstreamCalls.Load() != 1 {
		t.Errorf("upstream received %d calls, want 1", upstreamCalls.Load())
	}
}

// TestOCIAuthTransport_ProactiveFailureNoDoubleFetch tests that when a proactive token fetch
// fails and discovery finds the same challenge, the token endpoint is only hit once (not twice).
func TestOCIAuthTransport_ProactiveFailureNoDoubleFetch(t *testing.T) {
	var tokenCalls atomic.Int32

	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer tokenServer.Close()

	challenge := ociAuthChallenge{
		Realm:   tokenServer.URL,
		Service: "test-registry",
		Scope:   "repository:library/test:pull",
	}

	upstreamServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate",
			`Bearer realm="`+tokenServer.URL+`",service="test-registry",scope="repository:library/test:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstreamServer.Close()

	transport := newOCIAuthTransport(upstreamServer.Client().Transport)
	defer transport.Close()

	// Pre-seed the challenges map to trigger the proactive path
	upstreamURL, _ := url.Parse(upstreamServer.URL)
	key := challengeKey(upstreamURL.Host, "library/test")
	transport.challenges[key] = challengeEntry{
		challenge: challenge,
		expiry:    time.Now().Add(time.Hour),
	}

	ctx := testContext(t)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstreamServer.URL+"/v2/library/test/manifests/latest", nil)

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer resp.Body.Close()

	// Should get 401 (token endpoint is down, can't authenticate)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}

	// Token endpoint should be called exactly once (proactive path only),
	// not twice (proactive + discovery would be a double-fetch bug)
	if got := tokenCalls.Load(); got != 1 {
		t.Errorf("token endpoint called %d times, want 1 (double-fetch bug)", got)
	}
}
