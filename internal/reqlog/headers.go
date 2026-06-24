package reqlog

import (
	"log/slog"
	"net/http"
	"strings"
)

// HeaderAttrs returns a slog group attribute named group containing only the
// allowlisted headers present in h, with multi-value headers joined by ", ".
// When no allowlisted header is present the group has no attributes and slog
// handlers omit it entirely.
func HeaderAttrs(group string, h http.Header) slog.Attr {
	attrs := make([]any, 0, len(safeHeaders))

	for _, name := range safeHeaders {
		if vals := h.Values(name); len(vals) > 0 {
			attrs = append(attrs, slog.String(name, strings.Join(vals, ", ")))
		}
	}

	return slog.Group(group, attrs...)
}

// safeHeaders are HTTP headers we're pretty sure are safe to log. We allowlist
// because we don't want to log sensitive data (e.g. Authorization,
// X-Custom-Auth), and there's no telling what ends up in an arbitrary header.
// Names are in http.CanonicalHeaderKey form.
var safeHeaders = []string{
	"Accept",
	"Accept-Encoding",
	"User-Agent",
	"Content-Type",
	"Content-Length",
	"Range",
	"If-Range",
	"If-None-Match",
	"If-Modified-Since",
	"Etag",
	"Last-Modified",
	"Location",
}
