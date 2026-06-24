package reqlog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
)

// renderAttr logs a single attribute through a JSON handler and returns the
// parsed group (or nil if the attribute was omitted), exercising both
// HeaderAttrs and slog's real rendering of the group it produces.
func renderAttr(t *testing.T, attr slog.Attr) map[string]any {
	t.Helper()

	var buf bytes.Buffer

	slog.New(slog.NewJSONHandler(&buf, nil)).Info("test", attr)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("parse log line: %v", err)
	}

	group, ok := record[attr.Key].(map[string]any)
	if !ok {
		return nil
	}

	return group
}

func TestHeaderAttrsAllowlistsSafeHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("User-Agent", "test-agent")
	h.Set("Authorization", "Bearer supersecret")
	h.Set("Cookie", "session=secret")
	h.Set("X-Api-Key", "leakme")

	group := renderAttr(t, HeaderAttrs("request_headers", h))

	if group["Accept"] != "application/json" {
		t.Errorf("expected Accept logged, got %v", group["Accept"])
	}

	if group["User-Agent"] != "test-agent" {
		t.Errorf("expected User-Agent logged, got %v", group["User-Agent"])
	}

	for _, secret := range []string{"Authorization", "Cookie", "X-Api-Key"} {
		if _, present := group[secret]; present {
			t.Errorf("sensitive header %q must not be logged, got %v", secret, group[secret])
		}
	}
}

func TestHeaderAttrsJoinsMultiValue(t *testing.T) {
	h := http.Header{}
	h.Add("Accept", "application/json")
	h.Add("Accept", "text/plain")

	group := renderAttr(t, HeaderAttrs("request_headers", h))

	if group["Accept"] != "application/json, text/plain" {
		t.Errorf("expected joined Accept values, got %v", group["Accept"])
	}
}

func TestHeaderAttrsOmitsEmptyGroup(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer supersecret")

	if group := renderAttr(t, HeaderAttrs("request_headers", h)); group != nil {
		t.Errorf("expected group omitted when no allowlisted headers present, got %v", group)
	}
}
