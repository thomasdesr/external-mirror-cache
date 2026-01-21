package main

import (
	"net/url"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// genURL generates valid URLs with scheme, host, path, and optional query.
func genURL() *rapid.Generator[*url.URL] {
	return rapid.Custom(func(t *rapid.T) *url.URL {
		scheme := rapid.SampledFrom([]string{"http", "https"}).Draw(t, "scheme")
		host := rapid.StringMatching(`[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)*`).Draw(t, "host")
		path := "/" + rapid.StringMatching(`[a-zA-Z0-9/_.-]*`).Draw(t, "path")
		query := rapid.StringMatching(`[a-zA-Z0-9=&_.-]*`).Draw(t, "query")

		return &url.URL{
			Scheme:   scheme,
			Host:     host,
			Path:     path,
			RawQuery: query,
		}
	})
}

func TestS3PathForIsPure(t *testing.T) {
	cache := &s3HTTPCache{
		bucket: "test-bucket",
		prefix: "cache",
	}

	rapid.Check(t, func(t *rapid.T) {
		u := genURL().Draw(t, "url")

		path1 := cache.s3PathFor(u)
		path2 := cache.s3PathFor(u)

		if path1 != path2 {
			t.Fatalf("s3PathFor is not pure: %q != %q for URL %v", path1, path2, u)
		}
	})
}

func TestS3PathForDeterministic(t *testing.T) {
	cache1 := &s3HTTPCache{bucket: "bucket", prefix: "prefix"}
	cache2 := &s3HTTPCache{bucket: "bucket", prefix: "prefix"}

	rapid.Check(t, func(t *rapid.T) {
		u := genURL().Draw(t, "url")

		path1 := cache1.s3PathFor(u)
		path2 := cache2.s3PathFor(u)

		if path1 != path2 {
			t.Fatalf("s3PathFor is not deterministic across instances: %q != %q", path1, path2)
		}
	})
}

func TestS3PathForContainsHostAndPath(t *testing.T) {
	cache := &s3HTTPCache{
		bucket: "test-bucket",
		prefix: "cache",
	}

	rapid.Check(t, func(t *rapid.T) {
		u := genURL().Draw(t, "url")

		path := cache.s3PathFor(u)

		if len(path) == 0 {
			t.Fatal("s3PathFor returned empty string")
		}

		// Path should contain the host
		if u.Host != "" && !strings.Contains(path, u.Host) {
			t.Fatalf("s3PathFor result %q does not contain host %q", path, u.Host)
		}
	})
}

func TestS3PathForLeadingSlashStripped(t *testing.T) {
	cache := &s3HTTPCache{
		bucket: "test-bucket",
		prefix: "cache",
	}

	testCases := []struct {
		input    string
		expected string
	}{
		{"/file.txt", "cache/example.com/file.txt"},
		{"/dir/file.txt", "cache/example.com/dir/file.txt"},
		{"/", "cache/example.com/"},
	}

	for _, tc := range testCases {
		u := &url.URL{
			Scheme: "https",
			Host:   "example.com",
			Path:   tc.input,
		}

		got := cache.s3PathFor(u)
		if got != tc.expected {
			t.Errorf("s3PathFor(%v) = %q, want %q", u, got, tc.expected)
		}
	}
}

func TestS3PathForIncludesQuery(t *testing.T) {
	cache := &s3HTTPCache{
		bucket: "test-bucket",
		prefix: "cache",
	}

	testCases := []struct {
		path     string
		query    string
		expected string
	}{
		{"/dl", "json", "cache/example.com/dl?json"},
		{"/dl", "format=json&os=linux", "cache/example.com/dl?format%3Djson%26os%3Dlinux"},
		{"/file.txt", "", "cache/example.com/file.txt"},
	}

	for _, tc := range testCases {
		u := &url.URL{
			Scheme:   "https",
			Host:     "example.com",
			Path:     tc.path,
			RawQuery: tc.query,
		}

		got := cache.s3PathFor(u)
		if got != tc.expected {
			t.Errorf("s3PathFor(%v) = %q, want %q", u, got, tc.expected)
		}
	}
}

func TestParseTargetURLIsPure(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		host := rapid.StringMatching(`[a-z][a-z0-9.-]*`).Draw(t, "host")
		path := rapid.StringMatching(`[a-zA-Z0-9/_.-]+`).Draw(t, "path")
		fullPath := "/" + host + "/" + path

		url1, err1 := parseTargetURL(fullPath, "")
		url2, err2 := parseTargetURL(fullPath, "")

		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("parseTargetURL error inconsistency: %v vs %v", err1, err2)
		}

		if err1 == nil {
			if url1.String() != url2.String() {
				t.Fatalf("parseTargetURL is not pure: %q != %q", url1, url2)
			}
		}
	})
}

func TestParseTargetURLAlwaysHTTPS(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		host := rapid.StringMatching(`[a-z][a-z0-9.-]+`).Draw(t, "host")
		path := rapid.StringMatching(`[a-zA-Z0-9/_.-]+`).Draw(t, "path")
		fullPath := "/" + host + "/" + path

		u, err := parseTargetURL(fullPath, "")
		if err != nil {
			return // Invalid paths are fine, skip them
		}

		if u.Scheme != "https" {
			t.Fatalf("parseTargetURL should always produce https scheme, got %q", u.Scheme)
		}
	})
}

func TestParseTargetURLPreservesPath(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		host := rapid.StringMatching(`[a-z][a-z0-9.-]+`).Draw(t, "host")
		path := rapid.StringMatching(`[a-zA-Z0-9/_.-]+`).Draw(t, "path")
		fullPath := "/" + host + "/" + path

		u, err := parseTargetURL(fullPath, "")
		if err != nil {
			return
		}

		if u.Host != host {
			t.Fatalf("parseTargetURL did not preserve host: got %q, want %q", u.Host, host)
		}

		expectedPath := "/" + path
		if u.Path != expectedPath {
			t.Fatalf("parseTargetURL did not preserve path: got %q, want %q", u.Path, expectedPath)
		}
	})
}

func TestParseTargetURLPreservesQuery(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		host := rapid.StringMatching(`[a-z][a-z0-9.-]+`).Draw(t, "host")
		path := rapid.StringMatching(`[a-zA-Z0-9/_.-]+`).Draw(t, "path")
		query := rapid.StringMatching(`[a-zA-Z0-9=&_.-]*`).Draw(t, "query")
		fullPath := "/" + host + "/" + path

		u, err := parseTargetURL(fullPath, query)
		if err != nil {
			return
		}

		if u.RawQuery != query {
			t.Fatalf("parseTargetURL did not preserve query: got %q, want %q", u.RawQuery, query)
		}
	})
}

func TestParseTargetURLRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		host := rapid.StringMatching(`[a-z][a-z0-9.-]+`).Draw(t, "host")
		path := rapid.StringMatching(`[a-zA-Z0-9/_.-]+`).Draw(t, "path")
		query := rapid.StringMatching(`[a-zA-Z0-9=&_.-]*`).Draw(t, "query")
		fullPath := "/" + host + "/" + path

		u, err := parseTargetURL(fullPath, query)
		if err != nil {
			return
		}

		expectedURL := "https://" + host + "/" + path
		if query != "" {
			expectedURL += "?" + query
		}

		if u.String() != expectedURL {
			t.Fatalf("parseTargetURL round-trip failed: got %q, want %q", u.String(), expectedURL)
		}
	})
}
