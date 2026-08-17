package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"pgregory.net/rapid"
)

// spyUploader records the UploadObjectInput it receives.
type spyUploader struct {
	lastInput *transfermanager.UploadObjectInput
}

func (s *spyUploader) UploadObject(
	_ context.Context,
	input *transfermanager.UploadObjectInput,
	_ ...func(*transfermanager.Options),
) (*transfermanager.UploadObjectOutput, error) {
	s.lastInput = input

	return &transfermanager.UploadObjectOutput{}, nil
}

func TestPutSetsContentTypeOnUpload(t *testing.T) {
	spy := &spyUploader{}
	cache := &s3HTTPCache{
		s3u:    spy,
		bucket: "test-bucket",
		prefix: "cache",
	}

	headers := http.Header{
		"Content-Type": []string{"text/html"},
		"ETag":         []string{`"abc"`},
	}

	key := CacheKey{URL: &url.URL{
		Scheme: "https",
		Host:   "pypi.org",
		Path:   "/simple/azure-mgmt-dns/",
	}}

	err := cache.Put(context.Background(), key, headers, strings.NewReader("<html>packages</html>"))
	if err != nil {
		t.Fatalf("Put() error: %v", err)
	}

	if spy.lastInput.ContentType == nil || *spy.lastInput.ContentType != "text/html" {
		got := "<nil>"
		if spy.lastInput.ContentType != nil {
			got = *spy.lastInput.ContentType
		}

		t.Errorf("UploadObject ContentType = %s, want %q", got, "text/html")
	}
}

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

		path1 := cache.s3PathFor(CacheKey{URL: u})
		path2 := cache.s3PathFor(CacheKey{URL: u})

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

		path1 := cache1.s3PathFor(CacheKey{URL: u})
		path2 := cache2.s3PathFor(CacheKey{URL: u})

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

		path := cache.s3PathFor(CacheKey{URL: u})

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

		got := cache.s3PathFor(CacheKey{URL: u})
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

		got := cache.s3PathFor(CacheKey{URL: u})
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

// TestS3PathForEmptyVariantBackwardCompat verifies AC1.1: empty variant produces
// the same S3 path as the pre-refactoring URL-only behavior.
func TestS3PathForEmptyVariantBackwardCompat(t *testing.T) {
	cache := &s3HTTPCache{
		bucket: "test-bucket",
		prefix: "cache",
	}

	rapid.Check(t, func(t *rapid.T) {
		u := genURL().Draw(t, "url")
		key := CacheKey{URL: u}

		result := cache.s3PathFor(key)

		// Reconstruct expected path (URL-only behavior)
		expected := strings.Join([]string{cache.prefix, u.Host, strings.TrimPrefix(u.Path, "/")}, "/")
		if u.RawQuery != "" {
			expected += "?" + url.QueryEscape(u.RawQuery)
		}

		if result != expected {
			t.Fatalf("s3PathFor with empty variant: got %q, want %q", result, expected)
		}
	})
}

// TestS3PathForVariantAppendedWithSeparator verifies AC1.2: non-empty variant
// appends // + URL-escaped variant to the S3 path.
func TestS3PathForVariantAppendedWithSeparator(t *testing.T) {
	cache := &s3HTTPCache{
		bucket: "test-bucket",
		prefix: "cache",
	}

	testCases := []struct {
		name           string
		variant        string
		expectedSuffix string
	}{
		{"simple media type", "text/plain", "//text%2Fplain"},
		{"oci image index", "application/vnd.oci.image.index.v1+json", "//application%2Fvnd.oci.image.index.v1+json"},
		{"comma and space", "a, b", "//a%2C%20b"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			u := &url.URL{
				Scheme: "https",
				Host:   "example.com",
				Path:   "/api/manifests",
			}

			key := CacheKey{URL: u, Variant: tc.variant}
			result := cache.s3PathFor(key)

			if !strings.HasSuffix(result, tc.expectedSuffix) {
				t.Errorf("s3PathFor with Variant %q: got %q, want suffix %q", tc.variant, result, tc.expectedSuffix)
			}
		})
	}
}

// TestS3PathForSpecialCharactersEscaped verifies AC1.3: variants with special
// characters are included in the S3 path and where appropriate, are URL-escaped.
func TestS3PathForSpecialCharactersEscaped(t *testing.T) {
	cache := &s3HTTPCache{
		bucket: "test-bucket",
		prefix: "cache",
	}

	testCases := []struct {
		name           string
		variant        string
		expectedSuffix string
	}{
		{
			"forward slash escaped",
			"application/json",
			"//application%2Fjson",
		},
		{
			"plus not escaped (reserved char)",
			"application/vnd.oci+json",
			"//application%2Fvnd.oci+json",
		},
		{
			"colon not escaped (reserved char)",
			"text:plain",
			"//text:plain",
		},
		{
			"space escaped",
			"accept header",
			"//accept%20header",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			u := &url.URL{
				Scheme: "https",
				Host:   "registry.example.com",
				Path:   "/v2/image/manifests",
			}

			key := CacheKey{URL: u, Variant: tc.variant}
			result := cache.s3PathFor(key)

			if !strings.HasSuffix(result, tc.expectedSuffix) {
				t.Errorf("s3PathFor with Variant %q: got %q, want suffix %q", tc.variant, result, tc.expectedSuffix)
			}
		})
	}
}

// fakeS3ObjectClient is a spy s3ObjectClient: HeadObject serves canned
// output, CopyObject records its input and returns a configurable error.
type fakeS3ObjectClient struct {
	headOutput *s3.HeadObjectOutput
	headErr    error

	copyInput *s3.CopyObjectInput
	copyCalls int
	copyErr   error
}

func (f *fakeS3ObjectClient) HeadObject(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return f.headOutput, f.headErr
}

func (f *fakeS3ObjectClient) CopyObject(
	_ context.Context, input *s3.CopyObjectInput, _ ...func(*s3.Options),
) (*s3.CopyObjectOutput, error) {
	f.copyCalls++
	f.copyInput = input

	return &s3.CopyObjectOutput{}, f.copyErr
}

var testKey = CacheKey{URL: &url.URL{Scheme: "https", Host: "example.com", Path: "/pkg.whl"}}

// TestHeadReturnsEntry verifies Head populates the cachedEntry from the
// HeadObject reply: headers from metadata, StoredAt from LastModified,
// ObjectETag and Size for the conditional touch.
func TestHeadReturnsEntry(t *testing.T) {
	storedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeS3ObjectClient{headOutput: &s3.HeadObjectOutput{
		Metadata:      map[string]string{"Cache-Control": `["max-age=60"]`},
		LastModified:  aws.Time(storedAt),
		ETag:          aws.String(`"s3-object-etag"`),
		ContentLength: aws.Int64(1024),
	}}
	cache := &s3HTTPCache{s3c: fake, bucket: "b", prefix: "cache"}

	entry, err := cache.Head(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Head() error: %v", err)
	}

	if got := entry.Headers.Get("Cache-Control"); got != "max-age=60" {
		t.Errorf("Headers[Cache-Control] = %q, want %q", got, "max-age=60")
	}

	if !entry.StoredAt.Equal(storedAt) {
		t.Errorf("StoredAt = %v, want %v", entry.StoredAt, storedAt)
	}

	if entry.ObjectETag != `"s3-object-etag"` {
		t.Errorf("ObjectETag = %q, want %q", entry.ObjectETag, `"s3-object-etag"`)
	}

	if entry.Size != 1024 {
		t.Errorf("Size = %d, want 1024", entry.Size)
	}
}

// TestTouchIssuesConditionalCopy verifies the copy is a metadata self-copy
// conditional on the Head-time object ETag, re-supplies ContentType (REPLACE
// resets system metadata), and serializes headers through the allowlist.
func TestTouchIssuesConditionalCopy(t *testing.T) {
	fake := &fakeS3ObjectClient{}
	cache := &s3HTTPCache{s3c: fake, bucket: "b", prefix: "cache"}
	entry := &cachedEntry{ObjectETag: `"v1"`, Size: 1024}
	headers := http.Header{
		"Cache-Control":           []string{"max-age=86400"},
		"Content-Type":            []string{"application/zip"},
		"Content-Security-Policy": []string{"default-src 'none'"}, // not allowlisted
	}

	if err := cache.Touch(context.Background(), testKey, entry, headers); err != nil {
		t.Fatalf("Touch() error: %v", err)
	}

	in := fake.copyInput
	if in == nil {
		t.Fatal("Touch issued no CopyObject")
	}

	if got, want := aws.ToString(in.Key), "cache/example.com/pkg.whl"; got != want {
		t.Errorf("Key = %q, want %q", got, want)
	}

	if got, want := aws.ToString(in.CopySource), "b/cache/example.com/pkg.whl"; got != want {
		t.Errorf("CopySource = %q, want %q", got, want)
	}

	if got := aws.ToString(in.CopySourceIfMatch); got != `"v1"` {
		t.Errorf("CopySourceIfMatch = %q, want %q", got, `"v1"`)
	}

	if in.MetadataDirective != types.MetadataDirectiveReplace {
		t.Errorf("MetadataDirective = %v, want REPLACE", in.MetadataDirective)
	}

	if got := aws.ToString(in.ContentType); got != "application/zip" {
		t.Errorf("ContentType = %q, want %q", got, "application/zip")
	}

	if _, ok := in.Metadata["Cache-Control"]; !ok {
		t.Errorf("Metadata missing Cache-Control: %v", in.Metadata)
	}

	if _, ok := in.Metadata["Content-Security-Policy"]; ok {
		t.Errorf("Metadata carries non-allowlisted header: %v", in.Metadata)
	}
}

// TestTouchSkipsOversizedObjects verifies rfc9111-freshness.AC5.5: entries
// above the CopyObject single-part limit issue no copy attempt at all.
func TestTouchSkipsOversizedObjects(t *testing.T) {
	fake := &fakeS3ObjectClient{}
	cache := &s3HTTPCache{s3c: fake, bucket: "b", prefix: "cache"}
	entry := &cachedEntry{ObjectETag: `"v1"`, Size: copyObjectSizeLimit + 1}

	if err := cache.Touch(context.Background(), testKey, entry, http.Header{}); err != nil {
		t.Fatalf("Touch() error: %v", err)
	}

	if fake.copyCalls != 0 {
		t.Errorf("Touch issued %d CopyObject calls for oversized object, want 0", fake.copyCalls)
	}
}

var errPreconditionFailed = errors.New("PreconditionFailed")

// TestTouchReturnsCopyErrors verifies a failed copy (e.g. the 412 from a
// lost race) surfaces as an error for the caller's fail-open handling.
func TestTouchReturnsCopyErrors(t *testing.T) {
	fake := &fakeS3ObjectClient{copyErr: errPreconditionFailed}
	cache := &s3HTTPCache{s3c: fake, bucket: "b", prefix: "cache"}
	entry := &cachedEntry{ObjectETag: `"v1"`, Size: 1}

	err := cache.Touch(context.Background(), testKey, entry, http.Header{})
	if err == nil {
		t.Fatal("Touch() = nil error, want the copy failure")
	}
}

// TestTouchEscapesCopySource verifies keys containing query markers stay
// inside the copy-source path instead of starting an S3 subresource query.
func TestTouchEscapesCopySource(t *testing.T) {
	fake := &fakeS3ObjectClient{}
	cache := &s3HTTPCache{s3c: fake, bucket: "b", prefix: "cache"}
	key := CacheKey{URL: &url.URL{Scheme: "https", Host: "example.com", Path: "/dl", RawQuery: "json"}}

	if err := cache.Touch(context.Background(), key, &cachedEntry{ObjectETag: `"v"`, Size: 1}, http.Header{}); err != nil {
		t.Fatalf("Touch() error: %v", err)
	}

	if got := aws.ToString(fake.copyInput.CopySource); strings.Contains(got, "?") {
		t.Errorf("CopySource %q contains unescaped '?'", got)
	}
}

// TestTouchEncodesPlusInCopySource: S3 applies query-style decoding to
// x-amz-copy-source, so a literal '+' reads as a space and the copy 404s.
// PyPI local-version wheels (torch-2.1.0+cpu) put '+' in cache keys.
func TestTouchEncodesPlusInCopySource(t *testing.T) {
	fake := &fakeS3ObjectClient{}
	cache := &s3HTTPCache{s3c: fake, bucket: "b", prefix: "cache"}
	key := CacheKey{URL: &url.URL{Scheme: "https", Host: "files.pythonhosted.org", Path: "/torch-2.1.0+cpu.whl"}}

	if err := cache.Touch(context.Background(), key, &cachedEntry{ObjectETag: `"v"`, Size: 1}, http.Header{}); err != nil {
		t.Fatalf("Touch() error: %v", err)
	}

	got := aws.ToString(fake.copyInput.CopySource)
	if strings.Contains(got, "+") {
		t.Errorf("CopySource %q contains literal '+', want %%2B", got)
	}

	if want := "b/cache/files.pythonhosted.org/torch-2.1.0%2Bcpu.whl"; got != want {
		t.Errorf("CopySource = %q, want %q", got, want)
	}
}
