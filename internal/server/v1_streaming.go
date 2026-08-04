package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/tphakala/alfred/internal/store"
)

// workflowUpdateTimeout is the maximum time a goroutine issuing a Temporal
// workflow update will block before giving up, even if Temporal never responds.
const workflowUpdateTimeout = 5 * time.Minute

// writeSSEError marshals an error message as JSON and writes an SSE "error"
// event. Falls back to a static payload if JSON marshalling fails.
func writeSSEError(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, logger *slog.Logger, sessionID, msg string) {
	payload, err := json.Marshal(map[string]string{"message": msg})
	if err != nil {
		logger.ErrorContext(ctx, "marshal error payload", "sessionId", sessionID, "error", err)
		payload = []byte(`{"message":"internal error"}`)
	}
	_, _ = w.Write([]byte("event: error\ndata: " + string(payload) + "\n\n"))
	flusher.Flush()
}

// writeSSEEvent writes a single SSE event to w.
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event SSEEvent) {
	safeType := sseReplacer.Replace(event.Type)
	safeData := strings.ReplaceAll(event.Data, "\r", "")
	data := strings.ReplaceAll(safeData, "\n", "\ndata: ")
	_, _ = w.Write([]byte("event: " + safeType + "\ndata: " + data + "\n\n"))
	flusher.Flush()
}

// streamWorkflowTurn sends a Temporal workflow update and streams SSE events
// back to the client until the update completes or the client disconnects.
//
//nolint:gocognit,gocyclo // SSE streaming with select loop is inherently complex
func (s *Server) streamWorkflowTurn(w http.ResponseWriter, r *http.Request, sess *store.Session, updateName string, updateArgs any) {
	ctx := r.Context()

	if s.deps.SessionBroker == nil || s.deps.TemporalClient == nil {
		writeProblem(w, http.StatusServiceUnavailable, "stream backend not available")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	sessionIDStr := sess.ID.String()
	eventCh := s.deps.SessionBroker.Subscribe(sessionIDStr)
	defer s.deps.SessionBroker.Unsubscribe(sessionIDStr, eventCh)

	setSSEHeaders(w)
	flusher.Flush()

	type updateResult struct {
		errMsg string
		err    error
	}
	updateDone := make(chan updateResult, 1)
	updateCtx, updateCancel := context.WithTimeout(ctx, workflowUpdateTimeout)
	defer updateCancel()
	go func() {
		defer updateCancel()
		handle, uErr := s.deps.TemporalClient.UpdateWorkflow(updateCtx, client.UpdateWorkflowOptions{
			WorkflowID:   sess.WorkflowID,
			UpdateName:   updateName,
			Args:         []any{updateArgs},
			WaitForStage: client.WorkflowUpdateStageAccepted,
		})
		if uErr != nil {
			updateDone <- updateResult{err: uErr}
			return
		}
		// Both SendMessageResponse and RetryMessageResponse have the same
		// JSON shape {TurnID: int, Error: string}. Deserialize into a
		// common struct to avoid a type switch.
		var resp struct {
			Error string
		}
		uErr = handle.Get(updateCtx, &resp)
		if uErr != nil {
			updateDone <- updateResult{err: uErr}
		} else {
			updateDone <- updateResult{errMsg: resp.Error}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-eventCh:
			if !ok {
				return
			}
			writeSSEEvent(w, flusher, event)
			if event.Type == "done" {
				return
			}
		case result := <-updateDone:
			if result.err != nil {
				s.logger.ErrorContext(ctx, "workflow update failed",
					"sessionId", sessionIDStr, "error", result.err)
				writeSSEError(ctx, w, flusher, s.logger, sessionIDStr, "workflow update failed")
				return
			}
			if result.errMsg != "" {
				s.logger.ErrorContext(ctx, "workflow update returned error",
					"sessionId", sessionIDStr, "error", result.errMsg)
				writeSSEError(ctx, w, flusher, s.logger, sessionIDStr, result.errMsg)
				return
			}
		drainLoop:
			for {
				select {
				case evt, ok := <-eventCh:
					if !ok {
						break drainLoop
					}
					writeSSEEvent(w, flusher, evt)
				default:
					break drainLoop
				}
			}
			_, _ = w.Write([]byte("event: done\ndata: {}\n\n"))
			flusher.Flush()
			return
		}
	}
}
