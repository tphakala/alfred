package server

import (
	"net/http"

	"github.com/tphakala/alfred/api"
	"github.com/tphakala/alfred/internal/server/apiv1gen"
)

// handleOpenAPIJSON serves the OpenAPI 3.1 contract as JSON. Unauthenticated.
func (s *Server) handleOpenAPIJSON(w http.ResponseWriter, r *http.Request) {
	spec, err := api.SpecJSON()
	if err != nil {
		s.logger.ErrorContext(r.Context(), "failed to render openapi spec", "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to render openapi spec")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(spec)
}

// handlePollerV1 handles GET /api/v1/poller.
// If the poller is not configured, it returns 503.
func (s *Server) handlePollerV1(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Poller == nil {
		writeProblem(w, http.StatusServiceUnavailable, "poller not available")
		return
	}
	st := s.deps.Poller.Status()
	p := apiv1gen.Poller{
		LastPolled:    ptr(st.LastPolled),
		SeenCount:     ptr(st.SeenCount),
		WorkflowCount: ptr(st.WorkflowCount),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := writeJSON(w, p); err != nil {
		s.logger.Error("encode poller response", "error", err)
	}
}

// handleEventsFirehose handles GET /api/v1/events.
// It is a live-only global SSE firehose; no backlog replay is performed
// because operations events are not persisted.
func (s *Server) handleEventsFirehose(w http.ResponseWriter, r *http.Request) {
	// Ticket auth must come first.
	if !s.tickets.redeem(r.URL.Query().Get("ticket")) {
		writeProblem(w, http.StatusUnauthorized, "invalid or expired ticket")
		return
	}

	if s.deps.Broker == nil {
		writeProblem(w, http.StatusServiceUnavailable, "event broker not available")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	ch, err := s.deps.Broker.Subscribe()
	if err != nil {
		writeProblem(w, http.StatusServiceUnavailable, "server is shutting down")
		return
	}
	defer s.deps.Broker.Unsubscribe(ch)

	setSSEHeaders(w)
	flusher.Flush()

	// seq continues from the Last-Event-ID resume point.
	// The firehose is live-only; there is no persisted backlog to replay.
	seq := parseLastEventID(r)

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			seq++
			writeSSEFrame(w, seq, ev.Type, ev.Data)
			flusher.Flush()
		}
	}
}

// handleRootIndex serves a small JSON index at the root and a problem+json 404
// for any other unmatched path. The frontend is a separate project; this
// backend is headless.
func (s *Server) handleRootIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeProblem(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeProblem(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"name":"alfred","apiVersion":"v1","openapi":"/api/v1/openapi.json"}`))
}
