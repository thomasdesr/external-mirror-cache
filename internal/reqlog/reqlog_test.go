package reqlog

import (
	"context"
	"log/slog"
	"regexp"
	"testing"
)

// TestNewRequestIDLength verifies that NewRequestID generates 16-character hex strings.
func TestNewRequestIDLength(t *testing.T) {
	id := NewRequestID()
	if len(id) != 16 {
		t.Errorf("expected 16-char ID, got %d: %q", len(id), id)
	}

	// Verify it's valid hex
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(id) {
		t.Errorf("expected valid hex, got: %q", id)
	}
}

// TestNewRequestIDUnique verifies that multiple calls produce unique values.
func TestNewRequestIDUnique(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := NewRequestID()
		if ids[id] {
			t.Errorf("duplicate request ID on iteration %d: %q", i, id)
		}
		ids[id] = true
	}
}

// TestFromContextWithoutLogger verifies that FromContext returns slog.Default() when no logger is stored.
func TestFromContextWithoutLogger(t *testing.T) {
	ctx := context.Background()
	logger := FromContext(ctx)
	if logger != slog.Default() {
		t.Error("expected FromContext to return slog.Default() when no logger in context")
	}
}

// TestFromContextWithLogger verifies that FromContext returns the stored logger.
func TestFromContextWithLogger(t *testing.T) {
	ctx := context.Background()
	storedLogger := slog.New(slog.NewTextHandler(nil, nil))
	ctx = WithLogger(ctx, storedLogger)

	logger := FromContext(ctx)
	if logger != storedLogger {
		t.Error("expected FromContext to return the stored logger")
	}
}

// TestWithLoggerStoresInContext verifies that WithLogger properly stores a logger.
func TestWithLoggerStoresInContext(t *testing.T) {
	ctx := context.Background()
	testLogger := slog.New(slog.NewTextHandler(nil, nil))
	ctx = WithLogger(ctx, testLogger)

	retrieved := FromContext(ctx)
	if retrieved != testLogger {
		t.Error("expected WithLogger to store logger in context")
	}
}
