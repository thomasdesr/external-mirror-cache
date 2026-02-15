package main

import (
	"fmt"
	"net/http"
	"testing"

	"pgregory.net/rapid"
)

// genETag generates valid ETag values (quoted strings or weak ETags).
func genETag() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		isWeak := rapid.Bool().Draw(t, "isWeak")
		value := rapid.StringMatching(`[a-zA-Z0-9-]+`).Draw(t, "etagValue")

		if isWeak {
			return `W/"` + value + `"`
		}

		return `"` + value + `"`
	})
}

// genHTTPDate generates valid HTTP date strings.
func genHTTPDate() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		days := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
		months := []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

		day := rapid.SampledFrom(days).Draw(t, "day")
		date := rapid.IntRange(1, 28).Draw(t, "date")
		month := rapid.SampledFrom(months).Draw(t, "month")
		year := rapid.IntRange(2000, 2030).Draw(t, "year")
		hour := rapid.IntRange(0, 23).Draw(t, "hour")
		minute := rapid.IntRange(0, 59).Draw(t, "min")
		sec := rapid.IntRange(0, 59).Draw(t, "sec")

		return fmt.Sprintf("%s, %02d %s %d %02d:%02d:%02d GMT",
			day, date, month, year, hour, minute, sec)
	})
}

func TestInjectCacheHeaders_ETagAddsIfNoneMatch(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		etag := genETag().Draw(t, "etag")

		cachedHeaders := make(http.Header)
		cachedHeaders.Set("etag", etag)

		req, _ := http.NewRequest(http.MethodGet, "http://example.com/file", nil)
		injectCacheHeadersIntoRequest(req, cachedHeaders)

		got := req.Header.Get("If-None-Match")
		if got != etag {
			t.Fatalf("expected If-None-Match %q, got %q", etag, got)
		}
	})
}

func TestInjectCacheHeaders_LastModifiedAddsIfModifiedSince(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		lastMod := genHTTPDate().Draw(t, "lastModified")

		cachedHeaders := make(http.Header)
		cachedHeaders.Set("Last-Modified", lastMod)

		req, _ := http.NewRequest(http.MethodGet, "http://example.com/file", nil)
		injectCacheHeadersIntoRequest(req, cachedHeaders)

		got := req.Header.Get("If-Modified-Since")
		if got != lastMod {
			t.Fatalf("expected If-Modified-Since %q, got %q", lastMod, got)
		}
	})
}

func TestInjectCacheHeaders_BothHeadersInjected(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		etag := genETag().Draw(t, "etag")
		lastMod := genHTTPDate().Draw(t, "lastModified")

		cachedHeaders := make(http.Header)
		cachedHeaders.Set("etag", etag)
		cachedHeaders.Set("Last-Modified", lastMod)

		req, _ := http.NewRequest(http.MethodGet, "http://example.com/file", nil)
		injectCacheHeadersIntoRequest(req, cachedHeaders)

		gotETag := req.Header.Get("If-None-Match")
		gotLastMod := req.Header.Get("If-Modified-Since")

		if gotETag != etag {
			t.Fatalf("expected If-None-Match %q, got %q", etag, gotETag)
		}

		if gotLastMod != lastMod {
			t.Fatalf("expected If-Modified-Since %q, got %q", lastMod, gotLastMod)
		}
	})
}

func TestInjectCacheHeaders_SkipsIfRequestHasETagHeader(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cachedETag := genETag().Draw(t, "cachedETag")
		existingETag := genETag().Draw(t, "existingETag")

		cachedHeaders := make(http.Header)
		cachedHeaders.Set("etag", cachedETag)

		req, _ := http.NewRequest(http.MethodGet, "http://example.com/file", nil)
		req.Header.Set("etag", existingETag)

		injectCacheHeadersIntoRequest(req, cachedHeaders)

		got := req.Header.Get("If-None-Match")
		if got != "" {
			t.Fatalf("expected If-None-Match to be empty when request has ETag, got %q", got)
		}
	})
}

func TestInjectCacheHeaders_EmptyCachedHeaders(t *testing.T) {
	cachedHeaders := make(http.Header)

	req, _ := http.NewRequest(http.MethodGet, "http://example.com/file", nil)
	injectCacheHeadersIntoRequest(req, cachedHeaders)

	if req.Header.Get("If-None-Match") != "" {
		t.Fatal("If-None-Match should not be set for empty cached headers")
	}

	if req.Header.Get("If-Modified-Since") != "" {
		t.Fatal("If-Modified-Since should not be set for empty cached headers")
	}
}

func TestInjectCacheHeaders_Idempotent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		etag := genETag().Draw(t, "etag")
		lastMod := genHTTPDate().Draw(t, "lastModified")

		cachedHeaders := make(http.Header)
		cachedHeaders.Set("etag", etag)
		cachedHeaders.Set("Last-Modified", lastMod)

		req, _ := http.NewRequest(http.MethodGet, "http://example.com/file", nil)

		injectCacheHeadersIntoRequest(req, cachedHeaders)
		firstIfNoneMatch := req.Header.Get("If-None-Match")
		firstIfModifiedSince := req.Header.Get("If-Modified-Since")

		injectCacheHeadersIntoRequest(req, cachedHeaders)
		secondIfNoneMatch := req.Header.Get("If-None-Match")
		secondIfModifiedSince := req.Header.Get("If-Modified-Since")

		if firstIfNoneMatch != secondIfNoneMatch {
			t.Fatalf("If-None-Match changed after second injection: %q -> %q", firstIfNoneMatch, secondIfNoneMatch)
		}

		if firstIfModifiedSince != secondIfModifiedSince {
			t.Fatalf("If-Modified-Since changed after second injection: %q -> %q", firstIfModifiedSince, secondIfModifiedSince)
		}
	})
}

func TestInjectCacheHeaders_PreservesOtherHeaders(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		etag := genETag().Draw(t, "etag")
		otherHeaderKey := rapid.SampledFrom([]string{"Accept", "User-Agent", "Authorization"}).Draw(t, "otherKey")
		otherHeaderValue := rapid.StringMatching(`[a-zA-Z0-9 /-]+`).Draw(t, "otherValue")

		cachedHeaders := make(http.Header)
		cachedHeaders.Set("etag", etag)

		req, _ := http.NewRequest(http.MethodGet, "http://example.com/file", nil)
		req.Header.Set(otherHeaderKey, otherHeaderValue)

		injectCacheHeadersIntoRequest(req, cachedHeaders)

		if req.Header.Get(otherHeaderKey) != otherHeaderValue {
			t.Fatalf("other header %q was modified: expected %q, got %q",
				otherHeaderKey, otherHeaderValue, req.Header.Get(otherHeaderKey))
		}
	})
}
