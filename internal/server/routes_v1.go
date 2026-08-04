package server

import (
	"context"
	"net/http"
	"time"

	"github.com/tphakala/alfred/internal/server/apiv1gen"
)

// readyzProbeTimeout is the per-probe deadline for GET /readyz dependency checks.
const readyzProbeTimeout = 3 * time.Second

// registerV1Routes mounts the /api/v1 surface and ops endpoints onto mux.
// SSE routes are mounted on the root mux because they authenticate via a
// short-lived ?ticket= rather than the bearer header (EventSource cannot send
// headers); they redeem the ticket inside the handler. All other /api/v1
// routes are mounted on an inner mux behind CORS plus bearer auth.
func (s *Server) registerV1Routes(mux *http.ServeMux) {
	// Ops endpoints: unversioned, unauthenticated.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Unauthenticated discovery. Wrapped in corsMiddleware so a separate-origin
	// browser client can read these responses cross-origin (no-op when no
	// origins are configured). Preflight OPTIONS is already handled by the
	// /api/v1/ catch-all below.
	mux.Handle("GET /api/v1/auth-info", corsMiddleware(s.deps.CORSOrigins, http.HandlerFunc(s.handleAuthInfoV1)))
	mux.Handle("GET /api/v1/openapi.json", corsMiddleware(s.deps.CORSOrigins, http.HandlerFunc(s.handleOpenAPIJSON)))

	// SSE routes: ticket-authenticated, on the root mux (outside bearer auth).
	mux.Handle("GET /api/v1/runs/{run}/events", corsMiddleware(s.deps.CORSOrigins, http.HandlerFunc(s.handleRunEvents)))
	mux.Handle("GET /api/v1/events", corsMiddleware(s.deps.CORSOrigins, http.HandlerFunc(s.handleEventsFirehose)))

	// Authenticated JSON API.
	api := http.NewServeMux()
	api.HandleFunc("POST /api/v1/auth/sse-tickets", s.handleCreateSSETicket)

	api.HandleFunc("GET /api/v1/runs", s.handleListRuns)
	api.HandleFunc("POST /api/v1/runs", s.handleCreateRun)
	api.HandleFunc("GET /api/v1/runs/{run}", s.handleGetRun)
	api.HandleFunc("DELETE /api/v1/runs/{run}", s.handleDeleteRun)
	api.HandleFunc("POST /api/v1/runs/{run}/cancel", s.handleCancelRun)
	api.HandleFunc("POST /api/v1/runs/{run}/terminate", s.handleTerminateRun)
	api.HandleFunc("POST /api/v1/runs/{run}/signal", s.handleSignalRun)
	api.HandleFunc("GET /api/v1/runs/{run}/messages", s.handleListRunMessages)
	api.HandleFunc("POST /api/v1/runs/{run}/messages", s.handleSendRunMessage)
	api.HandleFunc("POST /api/v1/runs/{run}/messages/retry", s.handleRetryRunMessage)
	api.HandleFunc("GET /api/v1/runs/{run}/approvals", s.handleListRunApprovals)
	api.HandleFunc("POST /api/v1/runs/{run}/approvals/{callId}/resolve", s.handleResolveRunApproval)
	api.HandleFunc("POST /api/v1/runs/{run}/guidance", s.handleRunGuidance)
	api.HandleFunc("PUT /api/v1/runs/{run}/policy", s.handleSetRunPolicy)
	api.HandleFunc("PATCH /api/v1/runs/{run}/policy", s.handleExtendRunPolicy)
	api.HandleFunc("GET /api/v1/runs/{run}/children", s.handleListRunChildren)

	api.HandleFunc("GET /api/v1/workflow-configs", s.handleListWorkflowConfigs)
	api.HandleFunc("GET /api/v1/workflow-configs/{name}", s.handleGetWorkflowConfig)
	api.HandleFunc("PUT /api/v1/workflow-configs/{name}", s.handlePutWorkflowConfig)

	api.HandleFunc("GET /api/v1/agent-tasks", s.handleListAgentTasksV1)
	api.HandleFunc("GET /api/v1/agent-tasks/{name}", s.handleGetAgentTaskV1)
	api.HandleFunc("POST /api/v1/agent-tasks/{name}/run", s.handleRunAgentTaskV1)
	api.HandleFunc("GET /api/v1/agent-tasks/{task}/state", s.handleListTaskState)
	api.HandleFunc("POST /api/v1/agent-tasks/{task}/state/{key}/requeue", s.handleRequeueTaskState)

	api.HandleFunc("GET /api/v1/poller", s.handlePollerV1)

	mux.Handle("/api/v1/", corsMiddleware(s.deps.CORSOrigins, AuthMiddleware(s.apiKey, api)))
}

// handleReadyz probes each registered dependency and returns 200 when all pass,
// or 503 application/problem+json on the first failure.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	for _, p := range s.deps.ReadinessProbes {
		if p.Check == nil {
			continue
		}
		if err := runReadinessProbe(r.Context(), p); err != nil {
			// The failing probe name stays in the log, not in the unauthenticated
			// response body, and the problem title stays stable (RFC 9457).
			s.logger.WarnContext(r.Context(), "readyz probe failed", "probe", p.Name, "error", err)
			writeProblem(w, http.StatusServiceUnavailable, "not ready")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// runReadinessProbe runs a single readiness probe under a bounded timeout
// derived from ctx. The timeout context is propagated from a parameter rather
// than captured, and defer cancel() makes it panic-safe.
func runReadinessProbe(ctx context.Context, p ReadinessProbe) error {
	ctx, cancel := context.WithTimeout(ctx, readyzProbeTimeout)
	defer cancel()
	return p.Check(ctx)
}

// handleAuthInfoV1 reports whether an API key is required. Unauthenticated.
func (s *Server) handleAuthInfoV1(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := writeJSON(w, apiv1gen.AuthInfo{Required: s.apiKey != ""}); err != nil {
		s.logger.ErrorContext(r.Context(), "encode auth-info", "error", err)
	}
}
