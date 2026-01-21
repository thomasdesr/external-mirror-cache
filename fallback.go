package main

import (
	"errors"
	"net"
	"net/http"
)

// FallbackPolicy controls when the proxy serves stale cached content
// instead of returning an error when upstream is unavailable.
type FallbackPolicy struct {
	OnConnectionError bool // timeouts, DNS failures, connection refused
	On5xx             bool // HTTP 500/502/503/504 from upstream
	OnAnyError        bool // any non-200/304 response
}

// ShouldFallback returns true if the policy allows serving stale content
// for the given error or HTTP status code.
func (p FallbackPolicy) ShouldFallback(err error, statusCode int) bool {
	if p.OnAnyError && (err != nil || (statusCode != http.StatusOK && statusCode != http.StatusNotModified)) {
		return true
	}
	if err != nil && p.OnConnectionError && isConnectionError(err) {
		return true
	}
	if statusCode >= 500 && statusCode <= 504 && p.On5xx {
		return true
	}
	return false
}

func isConnectionError(err error) bool {
	var netErr net.Error
	return err != nil && errors.As(err, &netErr)
}
