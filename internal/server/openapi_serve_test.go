package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tphakala/alfred/internal/server"
)

func TestOpenAPIJSONEndpoint(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/openapi.json", http.NoBody)
	// No Authorization header: this endpoint must be unauthenticated.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %q", ct)
	}

	var doc map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal response body: %v", err)
	}

	if got, ok := doc["openapi"]; !ok {
		t.Fatal("response JSON missing 'openapi' field")
	} else if got != "3.1.0" {
		t.Fatalf("expected openapi == '3.1.0', got %q", got)
	}

	if _, ok := doc["paths"]; !ok {
		t.Fatal("response body missing paths")
	}
}

func TestOpenAPIJSONCORSAllowsConfiguredOrigin(t *testing.T) {
	const origin = "http://localhost:5173"
	srv := newTestServer(t, &server.Deps{CORSOrigins: []string{origin}})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/openapi.json", http.NoBody)
	req.Header.Set("Origin", origin)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != origin {
		t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, origin)
	}
}
