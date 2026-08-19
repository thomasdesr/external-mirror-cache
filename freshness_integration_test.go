package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// newTestServerWithFreshness is newTestServer with the freshness gate on.
func newTestServerWithFreshness(upstream *httptest.Server, cache *fakeCache, freshnessCap time.Duration) *httptest.Server {
	return newTestServerWith(upstream, cache, func(m *cacheMiddleware) {
		m.honorFreshness = true
		m.freshnessCap = freshnessCap
	})
}

const testFreshETag = `"v1"`

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// mustGet303 performs one proxied GET and asserts the 303 redirect.
func mustGet303(t *testing.T, client *http.Client, url string) {
	t.Helper()

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET %s: status %d, want 303", url, resp.StatusCode)
	}
}

// cachedURLFor reconstructs the fakeCache key string for a proxied path.
func cachedURLFor(upstream *httptest.Server, path string) string {
	u, _ := url.Parse(upstream.URL)

	return "https://" + u.Host + path
}

// ageEntry rewinds a cached entry's stored-at so tests can cross freshness
// boundaries without sleeping.
func ageEntry(t *testing.T, cache *fakeCache, cachedURL string, by time.Duration) {
	t.Helper()

	entry := cache.get(cachedURL)
	if entry == nil {
		t.Fatalf("no cached entry for %s", cachedURL)
	}

	cache.mu.Lock()
	entry.storedAt = entry.storedAt.Add(-by)
	cache.mu.Unlock()
}

// TestFreshness_FreshHitSkipsUpstream is rfc9111-freshness.AC1.1: a cached
// entry within its declared lifetime is served via 303 with zero upstream
// requests.
func TestFreshness_FreshHitSkipsUpstream(t *testing.T) {
	var upstreamHits atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Cache-Control", "max-age=365000000, immutable, public")
		w.Header().Set("ETag", `"pkg-etag"`)
		w.Write([]byte("wheel bytes"))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServerWithFreshness(upstream, cache, 7*24*time.Hour)

	defer proxy.Close()

	client := noRedirectClient()
	target := proxy.URL + upstreamHostPath(upstream, "/pkg.whl")

	mustGet303(t, client, target) // miss: fetch + cache
	mustGet303(t, client, target) // fresh: served with no upstream call
	mustGet303(t, client, target)

	if hits := upstreamHits.Load(); hits != 1 {
		t.Errorf("upstream hits = %d, want 1 (fresh hits must not revalidate)", hits)
	}
}

// TestFreshness_RevalidateThenTouchRearmsWindow is rfc9111-freshness.AC1.6
// end-to-end (stale → revalidate → fresh again, no body re-upload) plus
// AC5.2's retention half: the 304 omits Cache-Control and the merged entry
// keeps its declaration.
func TestFreshness_RevalidateThenTouchRearmsWindow(t *testing.T) {
	var upstreamHits atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)

		if r.Header.Get("If-None-Match") == testFreshETag {
			// A 304 that omits Cache-Control: merge must retain the stored one.
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", testFreshETag)
		w.Write([]byte("body"))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServerWithFreshness(upstream, cache, 7*24*time.Hour)

	defer proxy.Close()

	client := noRedirectClient()
	target := proxy.URL + upstreamHostPath(upstream, "/artifact")
	cachedURL := cachedURLFor(upstream, "/artifact")

	mustGet303(t, client, target) // miss: 1 hit, 1 put

	ageEntry(t, cache, cachedURL, 2*time.Hour) // past max-age=3600

	mustGet303(t, client, target) // stale: revalidates (hit 2), 304 → touch

	if hits := upstreamHits.Load(); hits != 2 {
		t.Fatalf("upstream hits after revalidation = %d, want 2", hits)
	}

	if calls := cache.touchCalls.Load(); calls != 1 {
		t.Fatalf("touch calls = %d, want 1 (revalidation must re-arm the window)", calls)
	}

	if calls := cache.putCalls.Load(); calls != 1 {
		t.Fatalf("put calls = %d, want 1 (touch must not re-upload the body)", calls)
	}

	if got := cache.get(cachedURL).headers.Get("Cache-Control"); got != "max-age=3600" {
		t.Fatalf("merged Cache-Control = %q, want retained %q", got, "max-age=3600")
	}

	mustGet303(t, client, target) // fresh again: no further upstream traffic

	if hits := upstreamHits.Load(); hits != 2 {
		t.Errorf("upstream hits after re-armed window = %d, want 2", hits)
	}
}

// TestFreshness_HeaderUpgradePropagatesInPlace is rfc9111-freshness.AC1.8's
// upgrade half: a no-cache entry whose later 304 carries max-age is touched
// and served fresh on the next request.
func TestFreshness_HeaderUpgradePropagatesInPlace(t *testing.T) {
	var upstreamHits atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)

		if r.Header.Get("If-None-Match") == testFreshETag {
			w.Header().Set("Cache-Control", "max-age=86400") // policy upgraded
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("Cache-Control", "public, no-cache")
		w.Header().Set("ETag", testFreshETag)
		w.Write([]byte("registry metadata"))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServerWithFreshness(upstream, cache, 7*24*time.Hour)

	defer proxy.Close()

	client := noRedirectClient()
	target := proxy.URL + upstreamHostPath(upstream, "/registry.json")

	mustGet303(t, client, target) // miss: stored as no-cache
	mustGet303(t, client, target) // no-cache: revalidates; 304 upgrades → touch

	if calls := cache.touchCalls.Load(); calls != 1 {
		t.Fatalf("touch calls = %d, want 1 (upgrade must propagate in place)", calls)
	}

	mustGet303(t, client, target) // fresh under the upgraded policy

	if hits := upstreamHits.Load(); hits != 2 {
		t.Errorf("upstream hits = %d, want 2 (upgraded entry must serve fresh)", hits)
	}
}

// TestFreshness_HeaderDowngradeAgesOutWithZeroWrites is
// rfc9111-freshness.AC1.8's downgrade half: when the upstream drops to
// no-cache, the touch is skipped and the entry revalidates per-request.
func TestFreshness_HeaderDowngradeAgesOutWithZeroWrites(t *testing.T) {
	var upstreamHits atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)

		if r.Header.Get("If-None-Match") == testFreshETag {
			w.Header().Set("Cache-Control", "public, no-cache") // policy downgraded
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", testFreshETag)
		w.Write([]byte("body"))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServerWithFreshness(upstream, cache, 7*24*time.Hour)

	defer proxy.Close()

	client := noRedirectClient()
	target := proxy.URL + upstreamHostPath(upstream, "/artifact")
	cachedURL := cachedURLFor(upstream, "/artifact")

	mustGet303(t, client, target)
	ageEntry(t, cache, cachedURL, 2*time.Hour)
	mustGet303(t, client, target) // stale: revalidates; merged no-cache → no touch
	mustGet303(t, client, target) // still stale: revalidates again

	if calls := cache.touchCalls.Load(); calls != 0 {
		t.Errorf("touch calls = %d, want 0 (downgrade needs no write)", calls)
	}

	if hits := upstreamHits.Load(); hits != 3 {
		t.Errorf("upstream hits = %d, want 3 (downgraded entry revalidates per request)", hits)
	}
}

// TestFreshness_FlagOffIsTodaysBehavior is rfc9111-freshness.AC2.4: with the
// flag off an otherwise-fresh entry still revalidates, and no touch occurs.
func TestFreshness_FlagOffIsTodaysBehavior(t *testing.T) {
	var upstreamHits atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)

		if r.Header.Get("If-None-Match") == testFreshETag {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("Cache-Control", "max-age=365000000, immutable")
		w.Header().Set("ETag", testFreshETag)
		w.Write([]byte("body"))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServer(upstream, cache) // flag off

	defer proxy.Close()

	client := noRedirectClient()
	target := proxy.URL + upstreamHostPath(upstream, "/pkg.whl")

	mustGet303(t, client, target)
	mustGet303(t, client, target)
	mustGet303(t, client, target)

	if hits := upstreamHits.Load(); hits != 3 {
		t.Errorf("upstream hits = %d, want 3 (flag off must revalidate every request)", hits)
	}

	if calls := cache.touchCalls.Load(); calls != 0 {
		t.Errorf("touch calls = %d, want 0 (flag off must not write)", calls)
	}
}

// TestFreshness_NeverFreshEntriesNeverTouched is rfc9111-freshness.AC2.6:
// successful revalidations of a no-cache entry perform no cache write.
func TestFreshness_NeverFreshEntriesNeverTouched(t *testing.T) {
	var upstreamHits atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)

		if r.Header.Get("If-None-Match") == testFreshETag {
			w.Header().Set("Cache-Control", "public, no-cache")
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("Cache-Control", "public, no-cache")
		w.Header().Set("ETag", testFreshETag)
		w.Write([]byte("module registry"))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServerWithFreshness(upstream, cache, 7*24*time.Hour)

	defer proxy.Close()

	client := noRedirectClient()
	target := proxy.URL + upstreamHostPath(upstream, "/bazel_registry.json")

	mustGet303(t, client, target)
	mustGet303(t, client, target)
	mustGet303(t, client, target)

	if hits := upstreamHits.Load(); hits != 3 {
		t.Errorf("upstream hits = %d, want 3 (no-cache revalidates every request)", hits)
	}

	if calls := cache.touchCalls.Load(); calls != 0 {
		t.Errorf("touch calls = %d, want 0 (touching a never-fresh entry buys nothing)", calls)
	}

	if calls := cache.putCalls.Load(); calls != 1 {
		t.Errorf("put calls = %d, want 1", calls)
	}
}

// TestFreshness_CapBoundsDeclaredLifetime is rfc9111-freshness.AC3.1's
// integration half: an entry declaring a year is stale once past the cap.
func TestFreshness_CapBoundsDeclaredLifetime(t *testing.T) {
	var upstreamHits atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)

		if r.Header.Get("If-None-Match") == testFreshETag {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("Cache-Control", "max-age=365000000, immutable")
		w.Header().Set("ETag", testFreshETag)
		w.Write([]byte("body"))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServerWithFreshness(upstream, cache, time.Hour) // short cap

	defer proxy.Close()

	client := noRedirectClient()
	target := proxy.URL + upstreamHostPath(upstream, "/pkg.whl")
	cachedURL := cachedURLFor(upstream, "/pkg.whl")

	mustGet303(t, client, target)
	ageEntry(t, cache, cachedURL, 90*time.Minute) // past the cap, well inside max-age
	mustGet303(t, client, target)                 // cap-expired: must revalidate

	if hits := upstreamHits.Load(); hits != 2 {
		t.Errorf("upstream hits = %d, want 2 (cap must bound the declared year)", hits)
	}
}

// TestFreshness_CDNResidentAgeDoesNotDefeatCap is rfc9111-freshness.AC1.9:
// CDN-fronted hosts hold hot immutable content for weeks, so every response
// — first fetch and validating 304 alike — arrives with an Age far past the
// cap. The cap bounds time since our own validation, not CDN residency: the
// entry serves fresh right after storing, and a revalidation re-arms it.
func TestFreshness_CDNResidentAgeDoesNotDefeatCap(t *testing.T) {
	var upstreamHits atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Age", "1728000") // 20 days of CDN residency, cap is 7

		if r.Header.Get("If-None-Match") == testFreshETag {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("Cache-Control", "max-age=365000000, immutable")
		w.Header().Set("ETag", testFreshETag)
		w.Write([]byte("wheel bytes"))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServerWithFreshness(upstream, cache, 7*24*time.Hour)

	defer proxy.Close()

	client := noRedirectClient()
	target := proxy.URL + upstreamHostPath(upstream, "/pkg.whl")
	cachedURL := cachedURLFor(upstream, "/pkg.whl")

	mustGet303(t, client, target) // miss: stored carrying Age past the cap
	mustGet303(t, client, target) // fresh: resident time is seconds

	if hits := upstreamHits.Load(); hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (CDN Age must not count against the cap)", hits)
	}

	ageEntry(t, cache, cachedURL, 8*24*time.Hour) // resident past the cap

	mustGet303(t, client, target) // cap-expired: revalidates; aged 304 → touch

	if calls := cache.touchCalls.Load(); calls != 1 {
		t.Fatalf("touch calls = %d, want 1 (an aged 304 must still re-arm)", calls)
	}

	mustGet303(t, client, target) // fresh again under the re-armed window

	if hits := upstreamHits.Load(); hits != 2 {
		t.Errorf("upstream hits = %d, want 2 (re-armed entry must serve fresh)", hits)
	}
}

// TestFreshness_OCIVariantKeysCanBeFresh is rfc9111-freshness.AC3.3: an OCI
// Accept-variant entry with Vary: Accept can be fresh, and distinct Accept
// variants are evaluated independently.
func TestFreshness_OCIVariantKeysCanBeFresh(t *testing.T) {
	var upstreamHits atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Vary", "Accept")
		w.Header().Set("Content-Type", r.Header.Get("Accept"))
		w.Write([]byte("manifest for " + r.Header.Get("Accept")))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServerWithFreshness(upstream, cache, 7*24*time.Hour)

	defer proxy.Close()

	client := noRedirectClient()
	target := proxy.URL + upstreamHostPath(upstream, "/v2/library/app/manifests/latest")

	get := func(accept string) {
		t.Helper()

		req, _ := http.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Accept", accept)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}

		resp.Body.Close()

		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("status %d, want 303", resp.StatusCode)
		}
	}

	get(ociImageIndexMediaType) // miss for this variant
	get(ociImageIndexMediaType) // fresh: Vary Accept ⊆ {Accept}

	if hits := upstreamHits.Load(); hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (variant entry should be fresh)", hits)
	}

	get(ociImageManifestMediaType) // different variant: its own miss

	if hits := upstreamHits.Load(); hits != 2 {
		t.Errorf("upstream hits = %d, want 2 (distinct variants evaluated independently)", hits)
	}
}

// TestFreshness_MismatchedETagOn304SkipsTouch pins RFC 9111 §4.3.4's MUST
// NOT: a 304 whose ETag doesn't strong-match the stored one (an IMS-only
// revalidator answering for changed content) must not update the entry —
// touching would relabel the old body with the new validator and poison
// every future revalidation.
func TestFreshness_MismatchedETagOn304SkipsTouch(t *testing.T) {
	var upstreamHits atomic.Int32

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)

		if r.Header.Get("If-None-Match") == testFreshETag {
			// Content changed upstream, but this revalidator only checks
			// If-Modified-Since: it 304s while advertising the new ETag.
			w.Header().Set("Cache-Control", "max-age=3600")
			w.Header().Set("ETag", `"v2"`)
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", testFreshETag)
		w.Write([]byte("body v1"))
	}))
	defer upstream.Close()

	cache := newFakeCache()
	proxy := newTestServerWithFreshness(upstream, cache, 7*24*time.Hour)

	defer proxy.Close()

	client := noRedirectClient()
	target := proxy.URL + upstreamHostPath(upstream, "/artifact")
	cachedURL := cachedURLFor(upstream, "/artifact")

	mustGet303(t, client, target)
	ageEntry(t, cache, cachedURL, 2*time.Hour)
	mustGet303(t, client, target) // 304 with mismatched ETag: no touch

	if calls := cache.touchCalls.Load(); calls != 0 {
		t.Errorf("touch calls = %d, want 0 (mismatched validator must not update the entry)", calls)
	}

	if got := cache.get(cachedURL).headers.Get("ETag"); got != testFreshETag {
		t.Errorf("stored ETag = %q, want unchanged %q", got, testFreshETag)
	}

	mustGet303(t, client, target) // still stale: revalidates again

	if hits := upstreamHits.Load(); hits != 3 {
		t.Errorf("upstream hits = %d, want 3 (entry must keep revalidating)", hits)
	}
}
