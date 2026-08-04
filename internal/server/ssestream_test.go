package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseLastEventID(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/events", http.NoBody)
	req.Header.Set("Last-Event-ID", "7")
	if got := parseLastEventID(req); got != 7 {
		t.Fatalf("got %d", got)
	}
	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/events", http.NoBody)
	if got := parseLastEventID(req2); got != 0 {
		t.Fatalf("default got %d", got)
	}

	// Invalid header values must all resolve to the 0 offset.
	for _, v := range []string{"-1", "abc", "  "} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/events", http.NoBody)
		req.Header.Set("Last-Event-ID", v)
		if got := parseLastEventID(req); got != 0 {
			t.Fatalf("value %q: got %d, want 0", v, got)
		}
	}
}

func TestWriteSSEFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSSEFrame(rec, 5, "tool_call", `{"name":"x"}`)
	out := rec.Body.String()
	for _, want := range []string{"id: 5", "event: tool_call", `data: {"name":"x"}`} {
		if !strings.Contains(out, want) {
			t.Fatalf("frame missing %q in %q", want, out)
		}
	}
}
