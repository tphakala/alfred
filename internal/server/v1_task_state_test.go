package server_test

import (
	"net/http"
	"testing"

	"github.com/tphakala/alfred/internal/server"
)

func TestListTaskStateNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/agent-tasks/some-task/state")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %q", ct, contentTypeProblem)
	}
}

func TestRequeueTaskStateNilStoreReturns503(t *testing.T) {
	srv := newTestServer(t, &server.Deps{})
	rr := doRequest(t, srv, http.MethodPost, "/api/v1/agent-tasks/some-task/state/some-key/requeue")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
	if ct := rr.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %q", ct, contentTypeProblem)
	}
}
