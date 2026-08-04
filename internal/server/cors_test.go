package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORSPreflightAllowedOrigin(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := corsMiddleware([]string{"http://localhost:5173"}, next)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/api/v1/runs", http.NoBody)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Fatalf("allow-origin = %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Content-Type") {
		t.Fatalf("allow-headers missing Content-Type: %q", rec.Header().Get("Access-Control-Allow-Headers"))
	}
}

func TestCORSDisallowedOriginNoHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := corsMiddleware([]string{"http://localhost:5173"}, next)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/runs", http.NoBody)
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("must not set allow-origin for disallowed origin")
	}
}

func TestCORSNoOriginsIsPassThrough(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) })
	h := corsMiddleware(nil, next)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/api/v1/runs", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("empty origins must be a pass-through (next called)")
	}
}
