package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/thomasdesr/external-mirror-cache/internal/errorutil"
)

// headerToMetadata takes an http.Header and serializes it into something that's
// suitable for storing HTTP headers in an S3 object's metadata.
func headerToMetadata(headers http.Header) (map[string]string, error) {
	metadata := make(map[string]string)

	for k, values := range headers {
		if _, ok := metadataHeaderAllowlist[http.CanonicalHeaderKey(k)]; !ok {
			continue
		}

		metadataValue, err := json.Marshal(values)
		if err != nil {
			return nil, errorutil.Wrapf(err, "marshal metadata %s=%s", k, values)
		}

		metadata[k] = string(metadataValue)
	}

	return metadata, nil
}

// metadataHeaderAllowlist is the set of response headers (canonical form)
// worth persisting to S3 object metadata: the validators the mirror reads
// back for conditional requests (ETag, Last-Modified), the entity headers
// describing the stored bytes, and fields reserved for designed-but-unlanded
// consumers -- the RFC 9111 freshness gate reads Cache-Control/Age/Date/
// Expires/Vary and its follow-up upload-skip rider reads
// Docker-Content-Digest (docs/design-plans/2026-08-04-rfc9111-freshness.md).
// Everything else is dropped -- S3 caps user metadata at 2048 bytes, and full
// upstream header sets exceed it (PyPI's Warehouse endpoints carry a
// 1155-byte CSP), turning PutObject into a permanent 400 MetadataTooLarge.
var metadataHeaderAllowlist = map[string]struct{}{
	"Age":                   {},
	"Cache-Control":         {},
	"Content-Encoding":      {},
	"Content-Length":        {},
	"Content-Type":          {},
	"Date":                  {},
	"Docker-Content-Digest": {},
	"Etag":                  {},
	"Expires":               {},
	"Last-Modified":         {},
	"Vary":                  {},
}

// metadataToHeader converts an S3 object's metadata into an http.Header. This
// essentially reverses the process of headerToMetadata, accounting for the
// behavior of S3 (e.g. prefixing custom headers with "x-amz-meta-").
func metadataToHeader(metadata map[string]string) (http.Header, error) {
	headers := make(http.Header)

	for k, v := range metadata {
		var parsedHeaderValues []string
		if err := json.Unmarshal([]byte(v), &parsedHeaderValues); err != nil {
			return nil, errorutil.Wrapf(err, "unmarshal metadata %s=%s", k, v)
		}

		for _, h := range parsedHeaderValues {
			cleanedKey := strings.TrimPrefix(k, "x-amz-meta-")
			headers.Add(cleanedKey, h)
		}
	}

	return headers, nil
}
