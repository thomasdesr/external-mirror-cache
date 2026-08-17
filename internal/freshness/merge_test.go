package freshness_test

import (
	"maps"
	"net/http"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/thomasdesr/external-mirror-cache/internal/freshness"
)

// TestMergeCases pins the §4.3.4 merge rules the design names
// (rfc9111-freshness.AC5.2 at the function level).
func TestMergeCases(t *testing.T) {
	cases := []struct {
		name       string
		stored     http.Header
		validating http.Header
		want       http.Header
	}{
		{
			// A 304 omitting Cache-Control must not erase the freshness
			// declaration — replace-instead-of-merge would demote the entry
			// to per-request revalidation forever.
			name:       "omitted Cache-Control retained",
			stored:     h("Cache-Control", "max-age=86400", "Etag", `"a"`),
			validating: h("Etag", `"a"`),
			want:       h("Cache-Control", "max-age=86400", "Etag", `"a"`),
		},
		{
			name:       "provided fields replace stored",
			stored:     h("Cache-Control", "max-age=60", "Etag", `"a"`),
			validating: h("Cache-Control", "max-age=86400", "Etag", `"b"`),
			want:       h("Cache-Control", "max-age=86400", "Etag", `"b"`),
		},
		{
			// Age measures the CDN residency of a response long gone;
			// inheriting it would permanently shorten every re-armed window.
			name:       "stored Age never inherited",
			stored:     h("Cache-Control", "max-age=86400", "Age", "11866"),
			validating: h("Etag", `"a"`),
			want:       h("Cache-Control", "max-age=86400", "Etag", `"a"`),
		},
		{
			name:       "validating Age wins",
			stored:     h("Cache-Control", "max-age=86400", "Age", "11866"),
			validating: h("Age", "3"),
			want:       h("Cache-Control", "max-age=86400", "Age", "3"),
		},
		{
			// Expires−Date is only meaningful within one response: a stored
			// Date must never pair with a validating response's Expires.
			name:       "stored Date never inherited",
			stored:     h("Expires", "Sat, 01 Aug 2026 01:00:00 GMT", "Date", "Sat, 01 Aug 2026 00:00:00 GMT"),
			validating: h("Expires", "Sat, 08 Aug 2026 01:00:00 GMT"),
			want:       h("Expires", "Sat, 08 Aug 2026 01:00:00 GMT"),
		},
		{
			// A bodiless 304 cannot describe the stored body (RFC 9111 §3.2).
			name:       "body-describing fields frozen",
			stored:     h("Content-Length", "1024", "Content-Type", "application/zip", "Content-Encoding", "gzip"),
			validating: h("Content-Length", "0", "Content-Type", "text/html", "Cache-Control", "max-age=60"),
			want:       h("Content-Length", "1024", "Content-Type", "application/zip", "Content-Encoding", "gzip", "Cache-Control", "max-age=60"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := freshness.Merge(tc.stored, tc.validating)
			if !maps.EqualFunc(got, tc.want, slices.Equal) {
				t.Fatalf("Merge() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Property: merge totality. For every header key, the output side is fully
// determined by the rules: body fields from stored, Age/Date from validating
// or absent, everything else validating-wins-else-stored.
func TestMergeTotality(t *testing.T) {
	bodyFields := []string{"Content-Length", "Content-Encoding", "Content-Range", "Content-Type"}
	deliveryFields := []string{"Age", "Date"}
	contentFields := []string{"Cache-Control", "Etag", "Expires", "Last-Modified", "Vary"}
	pool := slices.Concat(bodyFields, deliveryFields, contentFields)

	genSide := func(t *rapid.T, label string) http.Header {
		hdr := make(http.Header)
		for _, k := range rapid.SliceOfDistinct(rapid.SampledFrom(pool), rapid.ID).Draw(t, label) {
			hdr.Set(k, rapid.StringMatching(`[a-z0-9=," -]{1,20}`).Draw(t, label+"/"+k))
		}

		return hdr
	}

	rapid.Check(t, func(t *rapid.T) {
		stored := genSide(t, "stored")
		validating := genSide(t, "validating")

		got := freshness.Merge(stored, validating)

		for _, k := range pool {
			var want []string

			switch {
			case slices.Contains(bodyFields, k):
				want = stored.Values(k)
			case slices.Contains(deliveryFields, k):
				want = validating.Values(k)
			case len(validating.Values(k)) > 0:
				want = validating.Values(k)
			default:
				want = stored.Values(k)
			}

			if !slices.Equal(got.Values(k), want) {
				t.Fatalf("Merge()[%s] = %v, want %v (stored=%v validating=%v)",
					k, got.Values(k), want, stored, validating)
			}
		}
	})
}

// Merge must not mutate its inputs: the stored headers are also the gate's
// input, and the validating headers are the live response's.
func TestMergeDoesNotMutateInputs(t *testing.T) {
	stored := h("Cache-Control", "max-age=60", "Age", "5")
	validating := h("Etag", `"a"`)

	freshness.Merge(stored, validating)

	if got := stored.Get("Age"); got != "5" {
		t.Errorf("stored Age mutated: %q", got)
	}

	if got := validating.Get("Cache-Control"); got != "" {
		t.Errorf("validating gained Cache-Control: %q", got)
	}
}
