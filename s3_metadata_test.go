package main

import (
	"net/http"
	"testing"

	"pgregory.net/rapid"
)

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

// genHeader generates a random http.Header with 0-10 keys, each with 1-5 values.
func genHeader() *rapid.Generator[http.Header] {
	return rapid.Custom(func(t *rapid.T) http.Header {
		h := make(http.Header)
		numKeys := rapid.IntRange(0, 10).Draw(t, "numKeys")

		for range numKeys {
			key := genHeaderKey().Draw(t, "key")
			numValues := rapid.IntRange(1, 5).Draw(t, "numValues")

			for range numValues {
				value := genHeaderValue().Draw(t, "value")
				h.Add(key, value)
			}
		}

		return h
	})
}

func TestHeaderRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		original := genHeader().Draw(t, "header")

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
		original := genHeader().Draw(t, "header")

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

func TestMetadataToHeaderIsPure(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		original := genHeader().Draw(t, "header")

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
