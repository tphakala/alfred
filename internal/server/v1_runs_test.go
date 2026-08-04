package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tphakala/alfred/internal/server"
)

// doRequestBody issues an authenticated POST request with a JSON body and
// returns the recorded response.
func doRequestBody(t *testing.T, srv *server.Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestMethodBody(t, srv, http.MethodPost, path, body)
}

// doRequestMethodBody issues an authenticated request with an explicit method
// and JSON body, returning the recorded response.
func doRequestMethodBody(t *testing.T, srv *server.Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestListRunsNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/runs")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

func TestGetRunInvalidIDReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/runs/not-a-uuid")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestGetRunNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/runs/"+uuid.New().String())
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

func TestCreateRunUnsupportedStrategyReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestBody(t, srv, "/api/v1/runs", `{"strategy":"fanout"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestCreateRunReactNilTemporalReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestBody(t, srv, "/api/v1/runs", `{"strategy":"react"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

// Cancel action tests.

func TestCancelRunInvalidIDReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodPost, "/api/v1/runs/not-a-uuid/cancel")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestCancelRunNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodPost, "/api/v1/runs/"+uuid.New().String()+"/cancel")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

// Terminate action tests.

func TestTerminateRunInvalidIDReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodPost, "/api/v1/runs/not-a-uuid/terminate")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestTerminateRunNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodPost, "/api/v1/runs/"+uuid.New().String()+"/terminate")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

// Messages action tests.

func TestListRunMessagesInvalidIDReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/runs/not-a-uuid/messages")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

func TestListRunMessagesNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/runs/"+uuid.New().String()+"/messages")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

func TestSendRunMessageInvalidIDReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestBody(t, srv, "/api/v1/runs/not-a-uuid/messages", `{"text":"hello"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

func TestSendRunMessageNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestBody(t, srv, "/api/v1/runs/"+uuid.New().String()+"/messages", `{"text":"hello"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

func TestRetryRunMessageInvalidIDReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestBody(t, srv, "/api/v1/runs/not-a-uuid/messages/retry", `{"text":"hello"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

func TestRetryRunMessageNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestBody(t, srv, "/api/v1/runs/"+uuid.New().String()+"/messages/retry", `{"text":"hello"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

// Signal action tests.

func TestSignalRunInvalidIDReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodPost, "/api/v1/runs/not-a-uuid/signal")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
}

func TestSignalRunNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodPost, "/api/v1/runs/"+uuid.New().String()+"/signal")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

// Approvals action tests.

func TestListRunApprovalsInvalidIDReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/runs/not-a-uuid/approvals")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

func TestListRunApprovalsNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/runs/"+uuid.New().String()+"/approvals")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

func TestResolveRunApprovalInvalidIDReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestBody(t, srv, "/api/v1/runs/not-a-uuid/approvals/call-1/resolve", `{"approved":true}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

func TestResolveRunApprovalNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestBody(t, srv, "/api/v1/runs/"+uuid.New().String()+"/approvals/call-1/resolve", `{"approved":true}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

// Guidance action tests.

func TestRunGuidanceInvalidIDReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestBody(t, srv, "/api/v1/runs/not-a-uuid/guidance", `{"text":"do this"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

func TestRunGuidanceNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestBody(t, srv, "/api/v1/runs/"+uuid.New().String()+"/guidance", `{"text":"do this"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

// Policy action tests.

func TestSetRunPolicyInvalidIDReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestMethodBody(t, srv, http.MethodPut, "/api/v1/runs/not-a-uuid/policy", `{"autoApproveAll":true}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

func TestSetRunPolicyNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestMethodBody(t, srv, http.MethodPut, "/api/v1/runs/"+uuid.New().String()+"/policy", `{"autoApproveAll":true}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

func TestExtendRunPolicyInvalidIDReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestMethodBody(t, srv, http.MethodPatch, "/api/v1/runs/not-a-uuid/policy", `{"addTools":["Bash"]}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

func TestExtendRunPolicyNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequestMethodBody(t, srv, http.MethodPatch, "/api/v1/runs/"+uuid.New().String()+"/policy", `{"addTools":["Bash"]}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

// Children action tests.

func TestListRunChildrenInvalidIDReturns400(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/runs/not-a-uuid/children")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

func TestListRunChildrenNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/runs/"+uuid.New().String()+"/children")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

// Run events SSE tests.

// mintTicket mints a real SSE ticket via the bearer-authed endpoint and returns
// the token string.
func mintTicket(t *testing.T, srv *server.Server) string {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/sse-tickets", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("mint ticket: status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var resp struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("mint ticket: decode response: %v", err)
	}
	if resp.Ticket == "" {
		t.Fatal("mint ticket: got empty ticket")
	}
	return resp.Ticket
}

// TestRunEventsNoTicketReturns401 verifies that a request with no ticket query
// parameter is rejected with 401 problem+json before any SSE headers are set.
func TestRunEventsNoTicketReturns401(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/runs/"+uuid.New().String()+"/events", http.NoBody)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

// TestRunEventsBogusTicketReturns401 verifies that a bogus ticket is rejected.
func TestRunEventsBogusTicketReturns401(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/runs/"+uuid.New().String()+"/events?ticket=nope", http.NoBody)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %s", ct, contentTypeProblem)
	}
}

// TestRunEventsValidTicketNilStoreReturns503 proves that a valid ticket is
// accepted (ticket auth passes) and the handler then hits the store-nil guard
// inside requireRun, returning 503. If the ticket were rejected the response
// would be 401 instead.
func TestRunEventsValidTicketNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	tok := mintTicket(t, srv)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/runs/"+uuid.New().String()+"/events?ticket="+tok, http.NoBody)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (ticket accepted, store nil guard should fire)",
			rr.Code, http.StatusServiceUnavailable)
	}
}

// TestRunEventsSingleUseTicket mints one ticket, redeems it via the 503 request
// above, then verifies that a second request with the same token returns 401.
func TestRunEventsSingleUseTicket(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	tok := mintTicket(t, srv)
	runPath := "/api/v1/runs/" + uuid.New().String() + "/events?ticket=" + tok

	// First use: ticket accepted, then 503 from the nil store guard.
	req1 := httptest.NewRequestWithContext(t.Context(), http.MethodGet, runPath, http.NoBody)
	rr1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusServiceUnavailable {
		t.Fatalf("first use: status = %d, want %d", rr1.Code, http.StatusServiceUnavailable)
	}

	// Second use: ticket already consumed, must be 401.
	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodGet, runPath, http.NoBody)
	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusUnauthorized {
		t.Fatalf("second use: status = %d, want %d", rr2.Code, http.StatusUnauthorized)
	}
}
