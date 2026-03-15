package main

import (
	"net/url"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestCacheKeyStringEmptyVariantReturnsURLString(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		u := genURL().Draw(t, "url")
		key := CacheKey{URL: u}

		got := key.String()
		want := u.String()

		if got != want {
			t.Fatalf("CacheKey.String() with empty Variant: got %q, want %q", got, want)
		}
	})
}

func TestCacheKeyStringNonEmptyVariantIncludesNullSeparator(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		u := genURL().Draw(t, "url")
		variant := rapid.StringMatching(`[a-zA-Z0-9/+: .-]+`).Draw(t, "variant")

		// Skip empty variant strings since we're testing non-empty
		if variant == "" {
			return
		}

		key := CacheKey{URL: u, Variant: variant}
		got := key.String()

		if !strings.Contains(got, "\x00") {
			t.Fatalf("CacheKey.String() with non-empty Variant: result %q does not contain null separator", got)
		}
	})
}

func TestCacheKeyStringDifferentVariantsProduceDifferentStrings(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		u := genURL().Draw(t, "url")
		variant1 := rapid.StringMatching(`[a-zA-Z0-9/+: .-]+`).Draw(t, "variant1")
		variant2 := rapid.StringMatching(`[a-zA-Z0-9/+: .-]+`).Draw(t, "variant2")

		// Skip if either is empty or they're the same
		if variant1 == "" || variant2 == "" || variant1 == variant2 {
			return
		}

		key1 := CacheKey{URL: u, Variant: variant1}
		key2 := CacheKey{URL: u, Variant: variant2}

		got1 := key1.String()
		got2 := key2.String()

		if got1 == got2 {
			t.Fatalf("Different variants should produce different strings: %q == %q", got1, got2)
		}
	})
}

func TestCacheKeyStringVariantContainedInResult(t *testing.T) {
	testCases := []struct {
		variant string
	}{
		{"text/plain"},
		{"application/vnd.oci.image.index.v1+json"},
		{"a, b"},
	}

	for _, tc := range testCases {
		u := &url.URL{
			Scheme: "https",
			Host:   "example.com",
			Path:   "/file.txt",
		}

		key := CacheKey{URL: u, Variant: tc.variant}
		got := key.String()

		// Check that the variant appears after the null separator
		parts := strings.Split(got, "\x00")
		if len(parts) != 2 {
			t.Errorf("CacheKey.String() with Variant %q: expected 2 parts separated by null, got %d", tc.variant, len(parts))
		}

		if len(parts) == 2 && parts[1] != tc.variant {
			t.Errorf("CacheKey.String() with Variant %q: variant part is %q, want %q", tc.variant, parts[1], tc.variant)
		}
	}
}
