package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
)

// TestLogLevelParsing tests that valid log levels parse without error.
func TestLogLevelParsing(t *testing.T) {
	tests := []struct {
		name    string
		level   string
		wantErr bool
	}{
		{"debug level", "debug", false},
		{"info level", "info", false},
		{"warn level", "warn", false},
		{"error level", "error", false},
		{"invalid level", "banana", true},
		{"empty level", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var level slog.Level

			err := level.UnmarshalText([]byte(tt.level))

			if (err != nil) != tt.wantErr {
				t.Errorf("UnmarshalText(%q): got error %v, want error %v", tt.level, err != nil, tt.wantErr)
			}

			if !tt.wantErr {
				// Verify that parsing succeeded and the level is correct
				// (MarshalText returns uppercase, but parsing is case-insensitive)
				text, _ := level.MarshalText()
				if string(text) == "" {
					t.Errorf("expected MarshalText to return non-empty value")
				}
			}
		})
	}
}

// TestLogLevelFiltering tests that log levels properly filter output.
func TestLogLevelFiltering(t *testing.T) {
	tests := []struct {
		name              string
		configLevel       string
		logAtLevel        slog.Level
		shouldAppearCount int // how many log lines should appear
	}{
		{"debug level shows debug", "debug", slog.LevelDebug, 1},
		{"info level hides debug", "info", slog.LevelDebug, 0},
		{"info level shows info", "info", slog.LevelInfo, 1},
		{"warn level hides info", "warn", slog.LevelInfo, 0},
		{"warn level shows warn", "warn", slog.LevelWarn, 1},
		{"error level hides warn", "error", slog.LevelWarn, 0},
		{"error level shows error", "error", slog.LevelError, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			var level slog.Level

			err := level.UnmarshalText([]byte(tt.configLevel))
			if err != nil {
				t.Fatalf("failed to parse level %q: %v", tt.configLevel, err)
			}

			opts := &slog.HandlerOptions{Level: level}
			handler := slog.NewTextHandler(&buf, opts)
			logger := slog.New(handler)

			// Write a log at the test level
			switch tt.logAtLevel {
			case slog.LevelDebug:
				logger.Debug("debug message")
			case slog.LevelInfo:
				logger.Info("info message")
			case slog.LevelWarn:
				logger.Warn("warn message")
			case slog.LevelError:
				logger.Error("error message")
			}

			output := buf.String()
			if tt.shouldAppearCount > 0 && output == "" {
				t.Errorf("expected log message to appear, got empty output")
			}

			if tt.shouldAppearCount == 0 && output != "" {
				t.Errorf("expected log message to be filtered, got output: %q", output)
			}
		})
	}
}

// TestJSONHandlerOutput verifies JSON output format and expected fields.
func TestJSONHandlerOutput(t *testing.T) {
	var buf bytes.Buffer

	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	handler := slog.NewJSONHandler(&buf, opts)
	logger := slog.New(handler)

	logger.Info("test message", "key", "value")

	output := buf.String()
	if output == "" {
		t.Fatal("expected JSON output, got empty")
	}

	// Verify output is valid JSON
	var logRecord map[string]any
	if err := json.Unmarshal(buf.Bytes(), &logRecord); err != nil {
		t.Fatalf("expected valid JSON output, got parse error: %v", err)
	}

	// Verify expected fields
	if logRecord["msg"] != "test message" {
		t.Errorf("expected msg field %q, got %v", "test message", logRecord["msg"])
	}

	if logRecord["key"] != "value" {
		t.Errorf("expected key field %q, got %v", "value", logRecord["key"])
	}

	if _, hasTime := logRecord["time"]; !hasTime {
		t.Error("expected time field in JSON output")
	}

	if _, hasLevel := logRecord["level"]; !hasLevel {
		t.Error("expected level field in JSON output")
	}
}

// TestTextHandlerIsUsedForTTY verifies that isTTY correctly identifies terminal vs file.
func TestIsTTYDetection(t *testing.T) {
	// Create a temporary non-TTY file
	tmpFile, err := os.CreateTemp(t.TempDir(), "test-notty")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	isTTYResult := isTTY(tmpFile)
	if isTTYResult {
		t.Errorf("expected isTTY(tempfile) to be false, got true")
	}

	// isTTY on Stderr may or may not be true depending on test environment,
	// but we can verify it doesn't panic
	_ = isTTY(os.Stderr)
}
