package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tphakala/alfred/internal/server"
)

func TestReadyz(t *testing.T) {
	alwaysOK := func(_ context.Context) error { return nil }
	alwaysFail := func(_ context.Context) error { return errors.New("boom") }

	tests := []struct {
		name            string
		probes          []server.ReadinessProbe
		wantStatus      int
		wantContentType string
		wantBodyContain string
	}{
		{
			name:            "no probes: always ready",
			probes:          nil,
			wantStatus:      http.StatusOK,
			wantContentType: "application/json",
			wantBodyContain: `"status":"ok"`,
		},
		{
			name: "all probes pass: ready",
			probes: []server.ReadinessProbe{
				{Name: "database", Check: alwaysOK},
				{Name: "temporal", Check: alwaysOK},
			},
			wantStatus:      http.StatusOK,
			wantContentType: "application/json",
			wantBodyContain: `"status":"ok"`,
		},
		{
			name: "one probe fails: not ready",
			probes: []server.ReadinessProbe{
				{Name: "database", Check: alwaysFail},
			},
			wantStatus:      http.StatusServiceUnavailable,
			wantContentType: contentTypeProblem,
			wantBodyContain: "not ready",
		},
		{
			name: "second probe fails: not ready",
			probes: []server.ReadinessProbe{
				{Name: "temporal", Check: alwaysOK},
				{Name: "database", Check: alwaysFail},
			},
			wantStatus:      http.StatusServiceUnavailable,
			wantContentType: contentTypeProblem,
			wantBodyContain: "not ready",
		},
		{
			name: "probe with nil check is skipped: ready",
			probes: []server.ReadinessProbe{
				{Name: "misconfigured"}, // Check is nil
			},
			wantStatus:      http.StatusOK,
			wantContentType: "application/json",
			wantBodyContain: `"status":"ok"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, &server.Deps{
				ReadinessProbes: tc.probes,
			})
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", http.NoBody)
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rr.Code, tc.wantStatus)
			}
			ct := rr.Header().Get("Content-Type")
			if ct != tc.wantContentType {
				t.Fatalf("Content-Type = %q, want %q", ct, tc.wantContentType)
			}
			if tc.wantBodyContain != "" && !strings.Contains(rr.Body.String(), tc.wantBodyContain) {
				t.Fatalf("body = %q, want it to contain %q", rr.Body.String(), tc.wantBodyContain)
			}
		})
	}
}
