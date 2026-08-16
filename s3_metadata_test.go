package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestHeaderToMetadata_PyPIWarehouseFitsS3Limit pins the MetadataTooLarge fix:
// PyPI's Warehouse endpoints (/pypi/*/json, /help/, /rss/) carry ~2.7KB of
// response headers -- dominated by a 1155-byte Content-Security-Policy and a
// 610-byte Permissions-Policy (sizes from the 2026-08-15 production
// investigation) -- which exceeds S3's 2048-byte user-metadata cap and made
// every PutObject fail with a 400, permanently uncaching those endpoints.
func TestHeaderToMetadata_PyPIWarehouseFitsS3Limit(t *testing.T) {
	headers := pypiWarehouseHeaders()

	// Precondition: the fixture reproduces the failing regime. Serializing
	// every header must exceed the S3 cap, or this test isn't testing the fix.
	rawSize := 0
	for k := range headers {
		rawSize += len(k) + len(strings.Join(headers.Values(k), ","))
	}

	if rawSize <= 2048 {
		t.Fatalf("fixture too small to reproduce MetadataTooLarge: %d bytes of raw headers, need > 2048", rawSize)
	}

	metadata, err := headerToMetadata(headers)
	if err != nil {
		t.Fatalf("headerToMetadata failed: %v", err)
	}

	if size := metadataSize(metadata); size >= 2048 {
		t.Fatalf("metadata is %d bytes, must stay under S3's 2048-byte cap; metadata: %v", size, metadata)
	}

	if _, ok := metadata["Content-Security-Policy"]; ok {
		t.Error("Content-Security-Policy must not be stored: it is never read back")
	}

	// Revalidation must keep working: validators survive the filter.
	if _, ok := metadata["Etag"]; !ok {
		t.Error("ETag must survive filtering or every request re-uploads")
	}

	if _, ok := metadata["Last-Modified"]; !ok {
		t.Error("Last-Modified must survive filtering or every request re-uploads")
	}
}

// metadataSize is the byte count S3 applies its 2KB user-metadata cap to:
// the sum of key and value lengths.
func metadataSize(metadata map[string]string) int {
	size := 0
	for k, v := range metadata {
		size += len(k) + len(v)
	}

	return size
}

// pypiWarehouseHeaders reconstructs the header set of a pypi.org/pypi/*/json
// response. Only the shape and two sizes come from the 2026-08-15 production
// investigation -- 26 headers, ~2.7KB serialized, CSP and Permissions-Policy
// padded to their measured 1155 and 610 bytes; every value is an invented
// placeholder, not captured data.
func pypiWarehouseHeaders() http.Header {
	h := make(http.Header)

	csp := "default-src 'none'; base-uri 'self'; block-all-mixed-content; " +
		"connect-src 'self' https://api.github.com/repos/ https://api.github.com/search/issues " +
		"https://gitlab.com/api/ https://analytics.python.org fastly-insights.com " +
		"*.fastly-insights.com *.ethicalads.io https://api.pwnedpasswords.com " +
		"https://cdn.jsdelivr.net/npm/mathjax@3.2.2/es5/sre/mathmaps/ " +
		"https://2p66nmmycsj3.statuspage.io; font-src 'self' fonts.gstatic.com; " +
		"form-action 'self' https://checkout.stripe.com; frame-ancestors 'none'; " +
		"frame-src 'none'; img-src 'self' https://warehouse-camo.ingress.cmh1.psfhosted.org/ " +
		"*.fastly-insights.com *.ethicalads.io ethicalads.blob.core.windows.net; " +
		"script-src 'self' https://analytics.python.org *.fastly-insights.com *.ethicalads.io"
	h.Set("Content-Security-Policy", padTo(csp, 1155))

	pp := "accelerometer=(), ambient-light-sensor=(), autoplay=(), battery=(), camera=(), " +
		"cross-origin-isolated=(), display-capture=(), document-domain=(), encrypted-media=(), " +
		"execution-while-not-rendered=(), execution-while-out-of-viewport=(), fullscreen=(), " +
		"geolocation=(), gyroscope=(), keyboard-map=(), magnetometer=(), microphone=(), midi=()"
	h.Set("Permissions-Policy", padTo(pp, 610))

	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", "45231")
	h.Set("ETag", `"kXjB2Z8yQ4vN0mP1sT6wRg"`)
	h.Set("Last-Modified", "Sat, 08 Aug 2026 11:22:33 GMT")
	h.Set("Cache-Control", "max-age=900, public")
	h.Set("Vary", "Accept-Encoding")
	h.Set("Date", "Sat, 15 Aug 2026 02:06:23 GMT")
	h.Set("Age", "0")
	h.Set("Server", "nginx/1.25.2")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Access-Control-Allow-Headers", "Content-Type, If-Match, If-Modified-Since, If-None-Match, If-Unmodified-Since")
	h.Set("Access-Control-Allow-Methods", "GET")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "X-PyPI-Last-Serial")
	h.Set("Access-Control-Max-Age", "86400")
	h.Set("Referrer-Policy", "origin-when-cross-origin")
	h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "deny")
	h.Set("X-XSS-Protection", "1; mode=block")
	h.Set("X-Pypi-Last-Serial", "31251983")
	h.Set("X-Served-By", "cache-iad-kiad7000021-IAD, cache-pao-kpao1770020-PAO")
	h.Set("X-Cache", "MISS, MISS")
	h.Set("X-Cache-Hits", "0, 0")
	h.Set("X-Timer", "S1786759583.123456,VS0,VE145")

	return h
}

// padTo extends s to exactly n bytes with a repeated filler so fixture header
// sizes match the investigation's measured sizes without transcribing full
// values.
func padTo(s string, n int) string {
	for len(s) < n {
		s += " https://padding.invalid"
	}

	return s[:n]
}

func TestHeaderToMetadata_KeepsOnlyAllowlistedHeaders(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		original := genMixedHeader().Draw(t, "header")

		metadata, err := headerToMetadata(original)
		if err != nil {
			t.Fatalf("headerToMetadata failed: %v", err)
		}

		for k := range metadata {
			if _, ok := metadataHeaderAllowlist[http.CanonicalHeaderKey(k)]; !ok {
				t.Fatalf("non-allowlisted header %q leaked into metadata", k)
			}
		}

		// Every allowlisted header present in the input survives.
		for k := range original {
			if _, ok := metadataHeaderAllowlist[http.CanonicalHeaderKey(k)]; ok {
				if _, present := metadata[http.CanonicalHeaderKey(k)]; !present {
					t.Fatalf("allowlisted header %q was dropped", k)
				}
			}
		}
	})
}

// TestHeaderToMetadata_RequiredHeadersSurvive pins the allowlist's required
// members independently of the allowlist variable (the property tests above
// draw from and assert against the map itself, so they cannot catch an entry
// being removed). The duplication of the list here is the point.
func TestHeaderToMetadata_RequiredHeadersSurvive(t *testing.T) {
	required := []string{
		// Read back for conditional requests and the etag-skip.
		"ETag", "Last-Modified",
		// Read by the RFC 9111 freshness design
		// (docs/design-plans/2026-08-04-rfc9111-freshness.md).
		"Cache-Control", "Age", "Date", "Expires", "Vary",
	}

	h := make(http.Header)
	for _, k := range required {
		h.Set(k, "value")
	}

	metadata, err := headerToMetadata(h)
	if err != nil {
		t.Fatalf("headerToMetadata failed: %v", err)
	}

	for _, k := range required {
		if _, ok := metadata[http.CanonicalHeaderKey(k)]; !ok {
			t.Errorf("required header %q must survive metadata filtering", k)
		}
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		original := genAllowlistedHeader().Draw(t, "header")

		metadata, err := headerToMetadata(original)
		if err != nil {
			t.Fatalf("headerToMetadata failed: %v", err)
		}

		recovered, err := metadataToHeader(metadata)
		if err != nil {
			t.Fatalf("metadataToHeader failed: %v", err)
		}

		if !headersEqual(original, recovered) {
			t.Fatalf("round-trip failed:\noriginal:  %v\nrecovered: %v", original, recovered)
		}
	})
}

func TestHeaderRoundTripWithAmzPrefix(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		original := genAllowlistedHeader().Draw(t, "header")

		metadata, err := headerToMetadata(original)
		if err != nil {
			t.Fatalf("headerToMetadata failed: %v", err)
		}

		// Simulate S3's behavior of prefixing custom metadata keys with "x-amz-meta-"
		prefixedMetadata := make(map[string]string)
		for k, v := range metadata {
			prefixedMetadata["x-amz-meta-"+k] = v
		}

		recovered, err := metadataToHeader(prefixedMetadata)
		if err != nil {
			t.Fatalf("metadataToHeader failed: %v", err)
		}

		if !headersEqual(original, recovered) {
			t.Fatalf("round-trip with prefix failed:\noriginal:  %v\nrecovered: %v", original, recovered)
		}
	})
}

// TestHeaderRoundTripWithS3LowercaseKeys pins the key shape aws-sdk-go-v2's
// HeadObject actually returns: metadata keys come back lowercased with the
// x-amz-meta- prefix already stripped.
func TestHeaderRoundTripWithS3LowercaseKeys(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		original := genAllowlistedHeader().Draw(t, "header")

		metadata, err := headerToMetadata(original)
		if err != nil {
			t.Fatalf("headerToMetadata failed: %v", err)
		}

		lowercased := make(map[string]string)
		for k, v := range metadata {
			lowercased[strings.ToLower(k)] = v
		}

		recovered, err := metadataToHeader(lowercased)
		if err != nil {
			t.Fatalf("metadataToHeader failed: %v", err)
		}

		if !headersEqual(original, recovered) {
			t.Fatalf("round-trip with lowercase keys failed:\noriginal:  %v\nrecovered: %v", original, recovered)
		}
	})
}

// TestHeaderRoundTripNonCanonicalKeys pins that a header map whose keys are
// not in canonical form (possible when a map is built directly rather than
// via Set/Add) still round-trips: values must come from the map entry itself,
// not a canonicalizing lookup that misses the non-canonical key.
func TestHeaderRoundTripNonCanonicalKeys(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		keys := rapid.SliceOfNDistinct(
			rapid.SampledFrom(slices.Sorted(maps.Keys(metadataHeaderAllowlist))),
			1, 5, rapid.ID,
		).Draw(t, "keys")

		original := make(http.Header)
		expected := make(http.Header)

		for _, k := range keys {
			value := genHeaderValue().Draw(t, "value")
			original[randomizeCase(t, k)] = []string{value}
			expected.Add(k, value)
		}

		metadata, err := headerToMetadata(original)
		if err != nil {
			t.Fatalf("headerToMetadata failed: %v", err)
		}

		recovered, err := metadataToHeader(metadata)
		if err != nil {
			t.Fatalf("metadataToHeader failed: %v", err)
		}

		if !headersEqual(expected, recovered) {
			t.Fatalf("non-canonical round-trip failed:\noriginal:  %v\nexpected:  %v\nrecovered: %v", original, expected, recovered)
		}
	})
}

func randomizeCase(t *rapid.T, s string) string {
	out := []byte(s)
	for i, c := range out {
		if rapid.Bool().Draw(t, "flip") {
			switch {
			case c >= 'a' && c <= 'z':
				out[i] = c - 'a' + 'A'
			case c >= 'A' && c <= 'Z':
				out[i] = c - 'A' + 'a'
			}
		}
	}

	return string(out)
}

// TestMetadataToHeaderReadsAnyKey pins the read path staying permissive: the
// existing S3 corpus was written before the allowlist and carries arbitrary
// headers, which must keep parsing so cached validators keep working.
func TestMetadataToHeaderReadsAnyKey(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		original := genMixedHeader().Draw(t, "header")

		metadata := make(map[string]string)
		for k := range original {
			metadata[k] = mustMarshalValues(t, original.Values(k))
		}

		recovered, err := metadataToHeader(metadata)
		if err != nil {
			t.Fatalf("metadataToHeader failed: %v", err)
		}

		if !headersEqual(original, recovered) {
			t.Fatalf("read path dropped or altered headers:\noriginal:  %v\nrecovered: %v", original, recovered)
		}
	})
}

func mustMarshalValues(t *rapid.T, values []string) string {
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}

	return string(encoded)
}

func TestMetadataToHeaderIsPure(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		original := genAllowlistedHeader().Draw(t, "header")

		metadata, err := headerToMetadata(original)
		if err != nil {
			t.Fatalf("headerToMetadata failed: %v", err)
		}

		recovered1, err := metadataToHeader(metadata)
		if err != nil {
			t.Fatalf("first metadataToHeader failed: %v", err)
		}

		recovered2, err := metadataToHeader(metadata)
		if err != nil {
			t.Fatalf("second metadataToHeader failed: %v", err)
		}

		if !headersEqual(recovered1, recovered2) {
			t.Fatalf("metadataToHeader is not pure:\ncall 1: %v\ncall 2: %v", recovered1, recovered2)
		}
	})
}

func TestEmptyHeaderRoundTrip(t *testing.T) {
	original := make(http.Header)

	metadata, err := headerToMetadata(original)
	if err != nil {
		t.Fatalf("headerToMetadata failed: %v", err)
	}

	if len(metadata) != 0 {
		t.Fatalf("expected empty metadata, got: %v", metadata)
	}

	recovered, err := metadataToHeader(metadata)
	if err != nil {
		t.Fatalf("metadataToHeader failed: %v", err)
	}

	if len(recovered) != 0 {
		t.Fatalf("expected empty header, got: %v", recovered)
	}
}

// genAllowlistedHeader generates headers whose keys all survive metadata
// filtering, for round-trip properties.
func genAllowlistedHeader() *rapid.Generator[http.Header] {
	return genHeaderFromKeys(rapid.SampledFrom(slices.Sorted(maps.Keys(metadataHeaderAllowlist))))
}

// genMixedHeader generates headers mixing allowlisted and arbitrary keys.
func genMixedHeader() *rapid.Generator[http.Header] {
	allowlisted := slices.Sorted(maps.Keys(metadataHeaderAllowlist))
	keyGen := rapid.OneOf(
		rapid.SampledFrom(allowlisted),
		genHeaderKey(),
	)

	return genHeaderFromKeys(keyGen)
}

// genHeaderKey generates valid HTTP header keys.
// HTTP header keys must be valid tokens (alphanumeric + some symbols, no spaces).
func genHeaderKey() *rapid.Generator[string] {
	return rapid.StringMatching(`[A-Za-z][A-Za-z0-9-]*`)
}

// genHeaderValue generates valid HTTP header values.
// Values can contain most printable characters except certain control chars.
func genHeaderValue() *rapid.Generator[string] {
	return rapid.StringMatching(`[ -~]*`)
}

// genHeaderFromKeys generates a random http.Header with 0-10 keys drawn from
// keyGen, each with 1-5 values.
func genHeaderFromKeys(keyGen *rapid.Generator[string]) *rapid.Generator[http.Header] {
	return rapid.Custom(func(t *rapid.T) http.Header {
		h := make(http.Header)
		numKeys := rapid.IntRange(0, 10).Draw(t, "numKeys")

		for range numKeys {
			key := keyGen.Draw(t, "key")
			numValues := rapid.IntRange(1, 5).Draw(t, "numValues")

			for range numValues {
				value := genHeaderValue().Draw(t, "value")
				h.Add(key, value)
			}
		}

		return h
	})
}

// headersEqual compares two http.Header values for equality.
// Headers are equal if they have the same keys (case-insensitive due to canonicalization)
// with the same values in the same order.
func headersEqual(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}

	for key, aValues := range a {
		bValues := b.Values(key)
		if len(aValues) != len(bValues) {
			return false
		}

		for i, av := range aValues {
			if av != bValues[i] {
				return false
			}
		}
	}

	return true
}
