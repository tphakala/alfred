package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tphakala/alfred/internal/server"
)

// The ops and discovery endpoints must be reachable without an Authorization header.
func TestV1OpsAndAuthInfoUnauthenticated(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	for _, path := range []string{"/healthz", "/readyz", "/api/v1/auth-info"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, rr.Code)
		}
	}
}

// An authenticated v1 route reaches its handler when the bearer token is valid.
// With no poller configured the handler returns 503, which still proves auth
// passed and routing reached the real handler (not 401 or 404).
func TestV1AuthenticatedRouteReachesHandler(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/poller")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

// An authenticated v1 route rejects a request with no bearer token.
func TestV1AuthenticatedRouteRequiresAuth(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/runs", http.NoBody)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

// GET / returns 200 with a JSON index body and application/json content type.
func TestRootIndexOK(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("GET / Content-Type = %q, want application/json", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"apiVersion":"v1"`) {
		t.Fatalf("GET / body = %q, want apiVersion:v1", body)
	}
}

// GET /no-such-path returns 404 with Content-Type application/problem+json.
func TestRootIndex404(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/no-such-path", http.NoBody)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /no-such-path status = %d, want 404", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Fatalf("GET /no-such-path Content-Type = %q, want application/problem+json", ct)
	}
}
