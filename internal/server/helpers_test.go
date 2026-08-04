package server_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tphakala/alfred/internal/server"
)

const testAPIKey = "test-key"

// contentTypeProblem is the RFC 9457 problem details media type.
const contentTypeProblem = "application/problem+json"

func newTestServer(t *testing.T, deps *server.Deps) *server.Server {
	t.Helper()
	if deps.Broker == nil {
		deps.Broker = server.NewSSEBroker(slog.New(slog.DiscardHandler))
	}
	return server.New(":0", testAPIKey, deps, slog.New(slog.DiscardHandler))
}

func doRequest(t *testing.T, srv *server.Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}
