package freshness

import (
	"net/http"
	"slices"
)

// Merge produces the header set to store after a successful revalidation,
// per RFC 9111 §4.3.4: fields the validating response provides replace their
// stored counterparts; fields it omits are retained. Two exceptions:
//
//   - Age and Date describe the response that carried them, not the content.
//     They are taken from the validating response when present and dropped
//     otherwise — never inherited from the stored entry. (No value is
//     synthesized for a dropped Date: the lifetime rule's Expires−storedAt
//     fallback already anchors to the touch time.)
//   - Body-describing fields (Content-Length, Content-Encoding,
//     Content-Range, Content-Type) always come from the stored entry — a
//     bodiless 304 cannot describe the stored body (RFC 9111 §3.2).
func Merge(stored, validating http.Header) http.Header {
	merged := stored.Clone()
	if merged == nil {
		merged = make(http.Header)
	}

	merged.Del("Age")
	merged.Del("Date")

	for name, values := range validating {
		canonical := http.CanonicalHeaderKey(name)
		if _, frozen := bodyFields[canonical]; frozen {
			continue
		}

		merged[canonical] = slices.Clone(values)
	}

	return merged
}

var bodyFields = map[string]struct{}{
	"Content-Length":   {},
	"Content-Encoding": {},
	"Content-Range":    {},
	"Content-Type":     {},
}
