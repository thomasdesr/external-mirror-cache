package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/thomasdesr/external-mirror-cache/internal/errorutil"
	"github.com/thomasdesr/external-mirror-cache/internal/singleflight"
)

// ociAuthChallenge represents a parsed OCI Bearer token challenge.
type ociAuthChallenge struct {
	Realm   string
	Service string
	Scope   string
}

// parseOCIAuthChallenge parses an OCI Bearer token challenge from an HTTP 401 response.
// It returns the parsed challenge and whether a valid OCI challenge was found.
// A valid OCI challenge must have status 401, a WWW-Authenticate header with Bearer scheme,
// and realm, service, and scope parameters.
func parseOCIAuthChallenge(resp *http.Response) (ociAuthChallenge, bool) {
	if resp.StatusCode != http.StatusUnauthorized {
		return ociAuthChallenge{}, false
	}

	// Get all WWW-Authenticate headers
	headers := resp.Header.Values("Www-Authenticate")

	for _, header := range headers {
		header = strings.TrimSpace(header)

		// Check for Bearer scheme (case-insensitive)
		// Per RFC 7235, scheme must be followed by whitespace or end of string
		const bearerScheme = "Bearer"

		schemeEnd := len(bearerScheme)

		// Check if header starts with "Bearer" (case-insensitive)
		if len(header) < schemeEnd {
			continue
		}

		if !strings.EqualFold(header[:schemeEnd], bearerScheme) {
			continue
		}

		// If there's more after the scheme, it must start with whitespace
		if len(header) > schemeEnd && header[schemeEnd] != ' ' && header[schemeEnd] != '\t' {
			continue
		}

		// Get the remainder after the scheme
		remainder := strings.TrimSpace(header[schemeEnd:])

		// Parse the challenge parameters
		challenge := parseAuthChallengeParams(remainder)

		// realm is required; service and scope are optional per the
		// Docker Registry Token Auth spec and RFC 6750.
		realm, hasRealm := challenge["realm"]
		if !hasRealm {
			return ociAuthChallenge{}, false
		}

		return ociAuthChallenge{
			Realm:   realm,
			Service: challenge["service"],
			Scope:   challenge["scope"],
		}, true
	}

	return ociAuthChallenge{}, false
}

// parseAuthChallengeParams parses comma-separated key=value pairs from an auth challenge.
// Keys are normalized to lowercase.
// Values may be quoted (per RFC 7235) and may contain escaped characters.
func parseAuthChallengeParams(params string) map[string]string {
	result := make(map[string]string)

	// Split by comma, handling quoted values
	parts := splitAuthParams(params)

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Find the '=' separator
		before, after, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}

		key := strings.TrimSpace(before)
		value := strings.TrimSpace(after)

		// Remove quotes and handle escaped characters
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
			// Unescape escaped quotes
			value = strings.ReplaceAll(value, `\"`, `"`)
		}

		// Normalize key to lowercase for case-insensitive matching
		result[strings.ToLower(key)] = value
	}

	return result
}

// splitAuthParams splits comma-separated parameters, respecting quoted values.
func splitAuthParams(params string) []string {
	var (
		parts   []string
		current strings.Builder
	)

	inQuotes := false
	escape := false

	for i := range len(params) {
		ch := params[i]

		if escape {
			current.WriteByte(ch)

			escape = false

			continue
		}

		switch ch {
		case '\\':
			escape = true

			current.WriteByte(ch)
		case '"':
			inQuotes = !inQuotes

			current.WriteByte(ch)
		case ',':
			if !inQuotes {
				parts = append(parts, current.String())
				current.Reset()
			} else {
				current.WriteByte(ch)
			}
		default:
			current.WriteByte(ch)
		}
	}

	if current.Len() > 0 {
		parts = append(parts, current.String())
	}

	return parts
}

// tokenEntry holds a cached OCI Bearer token and its expiry time.
type tokenEntry struct {
	token  string
	expiry time.Time
}

// tokenCache manages OCI Bearer tokens with TTL-based expiration.
// Keyed directly by ociAuthChallenge (comparable struct) to avoid
// string-concatenation collision risks.
type tokenCache struct {
	mu      sync.RWMutex
	entries map[ociAuthChallenge]tokenEntry
}

// newTokenCache creates a new token cache.
func newTokenCache() *tokenCache {
	return &tokenCache{
		entries: make(map[ociAuthChallenge]tokenEntry),
	}
}

// get retrieves a token from the cache.
// Returns ("", false) if the token is missing or expired.
// Expired entries are left for set/evict to clean up so the read path
// only needs a shared lock.
func (c *tokenCache) get(challenge ociAuthChallenge) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[challenge]
	if !ok {
		return "", false
	}

	if time.Now().After(entry.expiry) {
		return "", false
	}

	return entry.token, true
}

// set stores a token in the cache with the given expiry time.
// The caller is responsible for applying the 30-second TTL buffer before calling set.
func (c *tokenCache) set(challenge ociAuthChallenge, token string, expiry time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[challenge] = tokenEntry{
		token:  token,
		expiry: expiry,
	}
}

// sweep removes all expired entries from the cache.
func (c *tokenCache) sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for k, e := range c.entries {
		if now.After(e.expiry) {
			delete(c.entries, k)
		}
	}
}

// evict removes a token from the cache.
func (c *tokenCache) evict(challenge ociAuthChallenge) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, challenge)
}

var (
	errRealmNotHTTPS = errors.New("realm URL scheme is not https")
	errTokenMissing  = errors.New("token response missing token and access_token fields")
	errTokenEndpoint = errors.New("unexpected token endpoint status")
)

// tokenResponse represents the JSON response from an OCI token endpoint.
// Field names match the OCI/Docker token API, not Go conventions.
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// fetchToken fetches an anonymous OCI Bearer token from the challenge's realm endpoint.
// It constructs the token URL with service and scope query parameters, executes a GET request,
// and parses the JSON response.
func fetchToken(ctx context.Context, base http.RoundTripper, challenge ociAuthChallenge) (string, time.Time, error) {
	// Parse realm URL and merge service/scope parameters
	realmURL, err := url.Parse(challenge.Realm)
	if err != nil {
		return "", time.Time{}, errorutil.Wrap(err, "invalid realm URL")
	}

	// Defense-in-depth: only fetch tokens over HTTPS.
	// The egress proxy (smokescreen) blocks private IPs, but scheme
	// validation prevents exotic-scheme SSRF (file://, gopher://, etc.).
	if realmURL.Scheme != "https" {
		return "", time.Time{}, fmt.Errorf("%w: %q", errRealmNotHTTPS, realmURL.Scheme)
	}

	q := realmURL.Query()
	if challenge.Service != "" {
		q.Set("service", challenge.Service)
	}

	if challenge.Scope != "" {
		q.Set("scope", challenge.Scope)
	}

	realmURL.RawQuery = q.Encode()
	tokenURL := realmURL.String()

	// Create GET request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", time.Time{}, errorutil.Wrap(err, "failed to create token request")
	}

	// Execute request via base transport
	resp, err := base.RoundTrip(req)
	if err != nil {
		return "", time.Time{}, errorutil.Wrap(err, "failed to fetch token")
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close

	// Check response status
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, errorutil.Wrapf(
			errTokenEndpoint, "token endpoint returned %d", resp.StatusCode,
		)
	}

	// Decode JSON response
	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return "", time.Time{}, errorutil.Wrap(err, "failed to decode token response")
	}

	// Extract token, preferring token field, falling back to access_token
	token := tr.Token
	if token == "" {
		token = tr.AccessToken
	}

	if token == "" {
		return "", time.Time{}, errTokenMissing
	}

	// Compute expiry with 30-second buffer
	expiresIn := tr.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 60 // default to 60 seconds
	}

	bufferedDuration := time.Duration(expiresIn)*time.Second - 30*time.Second
	if bufferedDuration <= 0 {
		bufferedDuration = 5 * time.Second // use 5 seconds if nearly expired
	}

	expiry := time.Now().Add(bufferedDuration)

	return token, expiry, nil
}

// extractOCIRepository extracts the OCI repository name from a URL path.
// OCI Distribution API paths follow the pattern /v2/{repository}/{action}.
// The repository can contain slashes (e.g., "my-org/my-image").
// The action is one of: manifests/{ref}, blobs/{digest}, tags/list, blobs/uploads/...
// Returns ("", "", false) if the path doesn't match the OCI pattern.
func extractOCIRepository(u *url.URL) (host string, repo string, ok bool) {
	path := u.Path

	// Path must start with /v2/
	const prefix = "/v2/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}

	// Strip the /v2/ prefix
	remainder := path[len(prefix):]

	// If nothing after /v2/, it's not a valid repository path
	if remainder == "" {
		return "", "", false
	}

	// Find the last occurrence of /manifests/, /blobs/, or /tags/
	// Everything before that is the repository name
	lastActionIdx := -1

	actions := []string{"/manifests/", "/blobs/", "/tags/"}
	for _, action := range actions {
		idx := strings.LastIndex(remainder, action)
		if idx > lastActionIdx {
			lastActionIdx = idx
		}
	}

	if lastActionIdx < 0 {
		// No action found, invalid OCI path
		return "", "", false
	}

	repo = remainder[:lastActionIdx]
	if repo == "" {
		// Action found but no repository name, invalid
		return "", "", false
	}

	return u.Host, repo, true
}

// challengeKey returns the map key for a (host, repo) pair in the challenges map.
func challengeKey(host, repo string) string {
	return host + "/" + repo
}

// tokenFetcher wraps fetchToken with singleflight deduplication and caching.
type tokenFetcher struct {
	base    http.RoundTripper
	cache   *tokenCache
	flights singleflight.Group[string]
}

// newTokenFetcher creates a new tokenFetcher with a token cache.
func newTokenFetcher(base http.RoundTripper) *tokenFetcher {
	return &tokenFetcher{
		base:  base,
		cache: newTokenCache(),
	}
}

// getOrFetch retrieves a token from cache or fetches it via singleflight deduplication.
// Multiple concurrent calls with the same challenge will share a single fetch operation.
func (f *tokenFetcher) getOrFetch(ctx context.Context, challenge ociAuthChallenge) (string, error) {
	// Check cache first
	if cached, ok := f.cache.get(challenge); ok {
		return cached, nil
	}

	// Singleflight key: null-separated to avoid collisions from field values containing spaces.
	key := challenge.Realm + "\x00" + challenge.Service + "\x00" + challenge.Scope

	// Use singleflight to deduplicate concurrent fetches
	token, err, _ := f.flights.Do(key, func() (string, error) {
		token, expiry, err := fetchToken(ctx, f.base, challenge)
		if err != nil {
			return "", err
		}

		f.cache.set(challenge, token, expiry)

		return token, nil
	})
	if err != nil {
		return "", errorutil.Wrap(err, "oci auth token fetch")
	}

	return token, nil
}

// challengeEntry pairs a learned OCI challenge with an expiry time so the
// challenges map can be lazily swept and does not grow without bound.
type challengeEntry struct {
	challenge ociAuthChallenge
	expiry    time.Time
}

const challengeTTL = 1 * time.Hour

// ociAuthTransport is an http.RoundTripper that transparently handles OCI Bearer token challenges.
// It intercepts 401 responses with OCI Bearer challenges, fetches tokens, and retries requests.
type ociAuthTransport struct {
	base         http.RoundTripper
	fetcher      *tokenFetcher
	challenges   map[string]challengeEntry
	challengesMu sync.RWMutex
	stop         chan struct{}
}

const sweepInterval = 5 * time.Minute

// newOCIAuthTransport creates a new ociAuthTransport wrapping the given base transport.
// A background goroutine sweeps expired entries from both the challenge map and token
// cache every 5 minutes. Call Close to stop it.
func newOCIAuthTransport(base http.RoundTripper) *ociAuthTransport {
	t := &ociAuthTransport{
		base:       base,
		fetcher:    newTokenFetcher(base),
		challenges: make(map[string]challengeEntry),
		stop:       make(chan struct{}),
	}
	go t.sweepLoop()

	return t
}

// Close stops the background sweep goroutine.
func (t *ociAuthTransport) Close() {
	close(t.stop)
}

// sweepLoop periodically removes expired entries from both the challenge
// map and the token cache, bounding memory growth to entries created
// within the last TTL window.
func (t *ociAuthTransport) sweepLoop() {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t.sweepChallenges()
			t.fetcher.cache.sweep()
		case <-t.stop:
			return
		}
	}
}

func (t *ociAuthTransport) sweepChallenges() {
	now := time.Now()

	t.challengesMu.Lock()
	for k, e := range t.challenges {
		if now.After(e.expiry) {
			delete(t.challenges, k)
		}
	}
	t.challengesMu.Unlock()
}

// Compile-time check that ociAuthTransport implements http.RoundTripper.
var _ http.RoundTripper = (*ociAuthTransport)(nil)

// RoundTrip implements http.RoundTripper.
func (t *ociAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Bypass check: if request already has Authorization header, pass through
	if req.Header.Get("Authorization") != "" {
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			return nil, errorutil.Wrap(err, "oci auth bypass")
		}

		return resp, nil
	}

	// Extract OCI repository once; non-OCI paths pass through directly
	host, repo, ok := extractOCIRepository(req.URL)
	if !ok {
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			return nil, errorutil.Wrap(err, "oci auth passthrough")
		}

		return resp, nil
	}

	// Try proactive path: if we have a cached challenge for this repository, use it
	key := challengeKey(host, repo)

	t.challengesMu.RLock()
	entry, found := t.challenges[key]
	t.challengesMu.RUnlock()

	if found && time.Now().Before(entry.expiry) { //nolint:nestif // proactive auth flow has inherent branching
		challenge := entry.challenge

		token, err := t.fetcher.getOrFetch(req.Context(), challenge)
		if err == nil {
			clonedReq := req.Clone(req.Context())
			clonedReq.Header.Set("Authorization", "Bearer "+token)

			resp, err := t.base.RoundTrip(clonedReq)
			if err != nil {
				return resp, errorutil.Wrap(err, "oci auth proactive request")
			}

			// Check if we got a 401 (token is stale)
			if resp.StatusCode == http.StatusUnauthorized {
				if req.Method == http.MethodGet || req.Method == http.MethodHead {
					log.Printf("oci auth: stale token for %s/%s, re-discovering", host, repo)
					// Drain and close the 401 body to allow connection reuse
					_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
					_ = resp.Body.Close()

					// Evict stale token from both challenge map and token cache
					t.challengesMu.Lock()
					delete(t.challenges, key)
					t.challengesMu.Unlock()
					t.fetcher.cache.evict(challenge)

					// Fall through to discovery path (nil: stale token, not a fetch failure)
					return t.discoveryPath(req, host, repo, nil)
				}
			}

			return resp, nil
		}

		log.Printf("oci auth: token fetch failed for %s/%s: %v", host, repo, err)
		// Token fetch failed, fall through to discovery.
		// Pass the failed challenge so discoveryPath can skip a redundant
		// re-fetch if it discovers the same challenge parameters.
		return t.discoveryPath(req, host, repo, &challenge)
	}

	return t.discoveryPath(req, host, repo, nil)
}

// discoveryPath handles the discovery flow: send bare request, check for OCI challenge,
// fetch token if needed, and retry. If failedChallenge is non-nil, token fetching is
// skipped when the discovered challenge matches — this avoids double-fetching the same
// failing token endpoint when the proactive path already tried and failed.
func (t *ociAuthTransport) discoveryPath(req *http.Request, host, repo string, failedChallenge *ociAuthChallenge) (*http.Response, error) {
	// Only retry for idempotent methods
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		// Non-idempotent methods are not retried
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			return nil, errorutil.Wrap(err, "oci auth non-idempotent passthrough")
		}

		return resp, nil
	}

	// Send bare request
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, errorutil.Wrap(err, "oci auth discovery request")
	}

	// Check if we got a 401 with OCI challenge
	challenge, ok := parseOCIAuthChallenge(resp)
	if !ok {
		// Not an OCI challenge, return as-is
		return resp, nil
	}

	log.Printf("oci auth: discovered challenge for %s/%s (realm=%s)", host, repo, challenge.Realm)

	// Store the challenge for future proactive requests
	key := challengeKey(host, repo)

	t.challengesMu.Lock()
	t.challenges[key] = challengeEntry{
		challenge: challenge,
		expiry:    time.Now().Add(challengeTTL),
	}
	t.challengesMu.Unlock()

	// Skip re-fetching if the discovered challenge is identical to one that just failed
	if failedChallenge != nil && challenge == *failedChallenge {
		log.Printf("oci auth: skipping re-fetch for %s/%s (same challenge already failed)", host, repo)

		return resp, nil
	}

	// Try to fetch the token
	token, err := t.fetcher.getOrFetch(req.Context(), challenge)
	if err != nil {
		log.Printf("oci auth: token fetch failed for %s/%s: %v", host, repo, err)
		// Token fetch failed, return the original 401
		return resp, nil
	}

	// Drain and close the 401 body to allow connection reuse
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()

	// Retry with token
	clonedReq := req.Clone(req.Context())
	clonedReq.Header.Set("Authorization", "Bearer "+token)

	resp, err = t.base.RoundTrip(clonedReq)
	if err != nil {
		return nil, errorutil.Wrap(err, "oci auth retry with token")
	}

	return resp, nil
}
