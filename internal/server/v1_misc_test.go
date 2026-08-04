package server_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tphakala/alfred/internal/server"
)

// TestPollerNilDepsReturns503 verifies that GET /api/v1/poller returns 503 with
// a problem+json body when no poller is configured.
func TestPollerNilDepsReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/poller")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

// TestEventsFirehoseNoTicketReturns401 verifies that GET /api/v1/events with no
// ticket query parameter is rejected with 401 problem+json.
func TestEventsFirehoseNoTicketReturns401(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/events", http.NoBody)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

// TestEventsFirehoseBogusTicketReturns401 verifies that GET /api/v1/events with
// an invalid ticket is rejected with 401 problem+json.
func TestEventsFirehoseBogusTicketReturns401(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/events?ticket=nope", http.NoBody)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

// TestEventsFirehoseNilBrokerReturns503 verifies that a valid ticket is accepted
// but a nil Broker results in a 503 problem+json response.
// This proves the ticket was redeemed (otherwise we would get 401).
func TestEventsFirehoseNilBrokerReturns503(t *testing.T) {
	// Build the server directly with a nil Broker so the broker nil-guard fires.
	// newTestServer auto-fills Broker, so we call New directly here.
	srv := server.New(":0", "test-key", &server.Deps{}, slog.New(slog.DiscardHandler))

	// Mint a ticket using the bearer-auth endpoint.
	tok := mintTicket(t, srv)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/events?ticket="+tok, http.NoBody)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s",
			rr.Code, http.StatusServiceUnavailable, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}
