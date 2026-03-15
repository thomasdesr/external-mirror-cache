package reqlog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMiddlewareSetXRequestIDHeader verifies that Middleware sets X-Request-ID header.
func TestMiddlewareSetXRequestIDHeader(t *testing.T) {
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)

	handler.ServeHTTP(rec, req)

	id := rec.Header().Get("X-Request-ID")
	if id == "" {
		t.Error("expected X-Request-ID header to be set")
	}

	if len(id) != 16 {
		t.Errorf("expected 16-char X-Request-ID, got %d: %q", len(id), id)
	}
}

// TestMiddlewareLogsRequestStart verifies that Middleware logs request start event.
func TestMiddlewareLogsRequestStart(t *testing.T) {
	var buf bytes.Buffer
	oldDefault := slog.Default()
	defer func() {
		slog.SetDefault(oldDefault)
	}()

	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))

	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)

	handler.ServeHTTP(rec, req)

	output := buf.String()
	if output == "" {
		t.Error("expected logs to be written")
		return
	}

	// Parse the first log line (request start)
	var logRecord map[string]interface{}
	lines := bytes.Split(buf.Bytes(), []byte("\n"))
	if len(lines) < 2 {
		t.Error("expected at least 2 log lines (start, end)")
		return
	}

	if err := json.Unmarshal(lines[0], &logRecord); err != nil {
		t.Fatalf("failed to parse first log line as JSON: %v", err)
	}

	if logRecord["msg"] != "request started" {
		t.Errorf("expected 'request started' message, got %q", logRecord["msg"])
	}

	if logRecord["request_id"] == "" {
		t.Error("expected request_id in log record")
	}

	if logRecord["method"] != "GET" {
		t.Errorf("expected method GET, got %v", logRecord["method"])
	}

	if logRecord["path"] != "/test" {
		t.Errorf("expected path /test, got %v", logRecord["path"])
	}
}

// TestMiddlewareLogsRequestEnd verifies that Middleware logs request end event with status and duration.
func TestMiddlewareLogsRequestEnd(t *testing.T) {
	var buf bytes.Buffer
	oldDefault := slog.Default()
	defer func() {
		slog.SetDefault(oldDefault)
	}()

	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))

	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("response"))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)

	handler.ServeHTTP(rec, req)

	lines := bytes.Split(buf.Bytes(), []byte("\n"))
	if len(lines) < 2 {
		t.Error("expected at least 2 log lines")
		return
	}

	// Parse the second log line (request end)
	var logRecord map[string]interface{}
	if err := json.Unmarshal(lines[1], &logRecord); err != nil {
		t.Fatalf("failed to parse second log line as JSON: %v", err)
	}

	if logRecord["msg"] != "request completed" {
		t.Errorf("expected 'request completed' message, got %q", logRecord["msg"])
	}

	if status, ok := logRecord["status"].(float64); !ok || int(status) != 200 {
		t.Errorf("expected status 200, got %v", logRecord["status"])
	}

	if _, hasDuration := logRecord["duration"]; !hasDuration {
		t.Error("expected duration in log record")
	}
}

// TestMiddlewarePassesLoggerInContext verifies that the middleware stores a logger in context.
func TestMiddlewarePassesLoggerInContext(t *testing.T) {
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := FromContext(r.Context())
		if logger == slog.Default() {
			t.Error("expected non-default logger in context")
		}
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)

	handler.ServeHTTP(rec, req)
}

// TestStatusWriterCapturesWriteHeaderStatus verifies statusWriter captures status code.
func TestStatusWriterCapturesWriteHeaderStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}

	sw.WriteHeader(http.StatusNotFound)

	if sw.status != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, sw.status)
	}
}

// TestStatusWriterDefaultsStatusToOKOnWrite verifies that status defaults to 200 when Write is called without WriteHeader.
func TestStatusWriterDefaultsStatusToOKOnWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}

	sw.Write([]byte("hello"))

	if sw.status != http.StatusOK {
		t.Errorf("expected default status %d, got %d", http.StatusOK, sw.status)
	}
}

// TestStatusWriterPreservesExistingStatusOnWrite verifies that status is not overwritten if already set.
func TestStatusWriterPreservesExistingStatusOnWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}

	sw.WriteHeader(http.StatusCreated)
	sw.Write([]byte("data"))

	if sw.status != http.StatusCreated {
		t.Errorf("expected status to remain %d, got %d", http.StatusCreated, sw.status)
	}
}

// BenchmarkMiddleware provides a performance baseline for middleware overhead.
func BenchmarkMiddleware(b *testing.B) {
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		handler.ServeHTTP(rec, req)
	}
}
