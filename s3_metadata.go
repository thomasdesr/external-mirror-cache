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

	for k := range headers {
		metadataValue, err := json.Marshal(headers.Values(k))
		if err != nil {
			return nil, errorutil.Wrapf(err, "marshal metadata %s=%s", k, headers.Values(k))
		}

		metadata[k] = string(metadataValue)
	}

	return metadata, nil
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
