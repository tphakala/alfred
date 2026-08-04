package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	"github.com/tphakala/alfred/internal/server/apiv1gen"
	"github.com/tphakala/alfred/internal/store"
	wf "github.com/tphakala/alfred/internal/workflow"
)

// ptr returns a pointer to v. It is a small helper for populating the optional
// pointer fields on generated API DTOs.
func ptr[T any](v T) *T { return &v }

// strategyForKind maps a session kind to its run execution strategy. Unknown
// kinds default to react.
func strategyForKind(kind string) apiv1gen.RunStrategy {
	switch kind {
	case store.SessionKindChat:
		return apiv1gen.RunStrategyReact
	case store.SessionKindTicket:
		return apiv1gen.RunStrategyStepDag
	case store.SessionKindAgentTask:
		return apiv1gen.RunStrategyFanout
	case store.SessionKindMonitor:
		return apiv1gen.RunStrategyMonitor
	case store.SessionKindCase:
		return apiv1gen.RunStrategyReact
	default:
		return apiv1gen.RunStrategyReact
	}
}

// supportedActionsForKind returns the advisory action segments a run of the
// given kind supports. These are hints for clients; the lifecycle and approval
// handlers enforce the 409 responses for unsupported actions.
func supportedActionsForKind(kind string) []string {
	switch kind {
	case store.SessionKindChat:
		return []string{"messages", "messages/retry", "approvals", "cancel"}
	case store.SessionKindTicket:
		return []string{"cancel", "terminate", "signal"}
	case store.SessionKindAgentTask:
		return []string{"cancel", "guidance", "policy", "approvals", "children"}
	case store.SessionKindMonitor:
		return []string{"cancel"}
	case store.SessionKindCase:
		return []string{"cancel", "children"}
	default:
		return []string{"cancel"}
	}
}

// runFromSession maps a persisted session row to its API run representation.
//
//nolint:gocritic // value receiver for callers that hold a value.
func runFromSession(sess store.Session) apiv1gen.Run {
	epoch := sess.Epoch
	createTime := sess.CreatedAt
	updateTime := sess.UpdatedAt
	actions := supportedActionsForKind(sess.Kind)

	run := apiv1gen.Run{
		Id:               sess.ID,
		Kind:             sess.Kind,
		Status:           sess.Status,
		Strategy:         strategyForKind(sess.Kind),
		Epoch:            &epoch,
		CreateTime:       &createTime,
		UpdateTime:       &updateTime,
		SupportedActions: &actions,
	}
	if sess.ParentSession != nil {
		parent := "runs/" + sess.ParentSession.String()
		run.Parent = &parent
	}
	return run
}

// requireRun parses the run ID from the URL path, loads the backing session,
// and writes a problem response on failure. Returns nil if a response was
// written.
func (s *Server) requireRun(w http.ResponseWriter, r *http.Request) *store.Session {
	// Validate the path before checking dependency availability so a malformed
	// run id is a 400 regardless of store state.
	id, err := uuid.Parse(r.PathValue("run"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid run id")
		return nil
	}

	if s.deps.Store == nil {
		writeProblem(w, http.StatusServiceUnavailable, "store not available")
		return nil
	}

	sess, err := s.deps.Store.GetSession(r.Context(), id)
	if err != nil {
		if store.IsNotFound(err) {
			writeProblem(w, http.StatusNotFound, "run not found")
			return nil
		}
		s.logger.ErrorContext(r.Context(), "failed to get run", "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to get run")
		return nil
	}
	return sess
}

// handleListRuns handles GET /api/v1/runs.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeProblem(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	ctx := r.Context()
	query := r.URL.Query()

	status := query.Get("status")
	if status == "" {
		status = store.SessionStatusActive
	}

	pageSize := parsePageSize(query.Get("pageSize"))
	offset, ok := decodePageToken(query.Get("pageToken"))
	if !ok {
		writeProblem(w, http.StatusBadRequest, "invalid page token")
		return
	}

	sessions, err := s.deps.Store.ListSessions(ctx, status)
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to list runs", "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to list runs")
		return
	}

	// Best-effort in-memory filtering over the returned page.
	strategyFilter := query.Get("strategy")
	kindFilter := query.Get("kind")
	filtered := sessions[:0]
	for i := range sessions {
		sess := &sessions[i]
		if strategyFilter != "" && string(strategyForKind(sess.Kind)) != strategyFilter {
			continue
		}
		if kindFilter != "" && sess.Kind != kindFilter {
			continue
		}
		filtered = append(filtered, *sess)
	}

	runs := make([]apiv1gen.Run, 0, pageSize)
	var nextPageToken *string
	if offset < len(filtered) {
		end := offset + pageSize
		if end > len(filtered) {
			end = len(filtered)
		}
		page := filtered[offset:end]
		for i := range page {
			runs = append(runs, runFromSession(page[i]))
		}
		if end < len(filtered) {
			tok := encodePageToken(end)
			nextPageToken = &tok
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(apiv1gen.RunList{Items: runs, NextPageToken: nextPageToken}); err != nil {
		s.logger.ErrorContext(ctx, "encode response", "error", err)
	}
}

// handleGetRun handles GET /api/v1/runs/{run}.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	sess := s.requireRun(w, r)
	if sess == nil {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(runFromSession(*sess)); err != nil {
		s.logger.ErrorContext(r.Context(), "encode response", "error", err)
	}
}

// handleDeleteRun handles DELETE /api/v1/runs/{run}. It completes the session
// and best-effort cancels the backing workflow.
func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	sess := s.requireRun(w, r)
	if sess == nil {
		return
	}
	ctx := r.Context()

	if err := s.deps.Store.UpdateSessionStatus(ctx, sess.ID, store.SessionStatusCompleted); err != nil {
		s.logger.ErrorContext(ctx, "failed to update run status", "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to complete run")
		return
	}

	if s.deps.TemporalClient != nil {
		if err := s.deps.TemporalClient.CancelWorkflow(ctx, sess.WorkflowID, sess.RunID); err != nil {
			s.logger.WarnContext(ctx, "failed to cancel workflow (may already be done)",
				"workflowId", sess.WorkflowID, "error", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// runSupportsAction reports whether a run of the given kind supports the action segment.
func runSupportsAction(kind, action string) bool {
	return slices.Contains(supportedActionsForKind(kind), action)
}

// handleCreateRun handles POST /api/v1/runs. Only ad-hoc react (chat) runs can
// be created here; catalog agents use POST /api/v1/agent-tasks/{name}/run.
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req apiv1gen.CreateRunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&req); err != nil && err != io.EOF {
		writeProblem(w, http.StatusBadRequest, "invalid json body")
		return
	}

	// Only ad-hoc react runs are accepted here. A non-react strategy or an
	// agent reference means the caller wants a catalog agent.
	reactOnly := req.Strategy == nil || *req.Strategy == apiv1gen.CreateRunRequestStrategyReact
	if !reactOnly || req.Agent != nil {
		writeProblem(w, http.StatusBadRequest, "only ad-hoc react runs can be created here; use POST /api/v1/agent-tasks/{name}/run for catalog agents")
		return
	}

	if s.deps.TemporalClient == nil || s.deps.Store == nil {
		writeProblem(w, http.StatusServiceUnavailable, "run backend not available")
		return
	}

	sessionID := uuid.New()
	workflowID := "alfred-chat-" + sessionID.String()

	run, err := s.deps.TemporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: s.deps.ChatTaskQueue,
	}, wf.ChatWorkflow, wf.ChatWorkflowParams{
		SessionID:         sessionID,
		Epoch:             1,
		Kind:              store.SessionKindChat,
		AutoRecallEnabled: s.deps.AutoRecallEnabled,
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to start chat workflow", "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to start run workflow")
		return
	}

	sess := store.Session{
		ID:         sessionID,
		WorkflowID: workflowID,
		RunID:      run.GetRunID(),
		Kind:       store.SessionKindChat,
		Status:     store.SessionStatusActive,
		Epoch:      1,
	}
	if err := s.deps.Store.CreateSession(ctx, &sess); err != nil {
		// Cancel the orphaned workflow so it does not run without a row.
		if cancelErr := s.deps.TemporalClient.CancelWorkflow(ctx, workflowID, run.GetRunID()); cancelErr != nil {
			s.logger.WarnContext(ctx, "failed to cancel orphaned workflow",
				"workflowId", workflowID, "error", cancelErr)
		}
		s.logger.ErrorContext(ctx, "failed to persist run", "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to persist run")
		return
	}

	s.logger.InfoContext(ctx, "run created",
		"runId", sessionID.String(), "workflowId", workflowID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(runFromSession(sess)); err != nil {
		s.logger.ErrorContext(ctx, "encode response", "error", err)
	}
}

// handleCancelRun handles POST /api/v1/runs/{run}/cancel.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	sess := s.requireRun(w, r)
	if sess == nil {
		return
	}
	ctx := r.Context()

	if !runSupportsAction(sess.Kind, "cancel") {
		writeProblemType(w, http.StatusConflict, "cancel is not a supported action for this run", "unsupported-action")
		return
	}

	if s.deps.TemporalClient == nil {
		writeProblem(w, http.StatusServiceUnavailable, "temporal client not available")
		return
	}

	if err := s.deps.TemporalClient.CancelWorkflow(ctx, sess.WorkflowID, ""); err != nil {
		s.logger.ErrorContext(ctx, "failed to cancel workflow", "workflowId", sess.WorkflowID, "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to cancel run")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(runFromSession(*sess)); err != nil {
		s.logger.ErrorContext(ctx, "encode response", "error", err)
	}
}

// handleTerminateRun handles POST /api/v1/runs/{run}/terminate.
func (s *Server) handleTerminateRun(w http.ResponseWriter, r *http.Request) {
	sess := s.requireRun(w, r)
	if sess == nil {
		return
	}
	ctx := r.Context()

	if !runSupportsAction(sess.Kind, "terminate") {
		writeProblemType(w, http.StatusConflict, "terminate is not a supported action for this run", "unsupported-action")
		return
	}

	if s.deps.TemporalClient == nil {
		writeProblem(w, http.StatusServiceUnavailable, "temporal client not available")
		return
	}

	if err := s.deps.TemporalClient.TerminateWorkflow(ctx, sess.WorkflowID, "", "terminated via Alfred API"); err != nil {
		s.logger.ErrorContext(ctx, "failed to terminate workflow", "workflowId", sess.WorkflowID, "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to terminate run")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(runFromSession(*sess)); err != nil {
		s.logger.ErrorContext(ctx, "encode response", "error", err)
	}
}

// handleSignalRun handles POST /api/v1/runs/{run}/signal.
func (s *Server) handleSignalRun(w http.ResponseWriter, r *http.Request) {
	sess := s.requireRun(w, r)
	if sess == nil {
		return
	}
	ctx := r.Context()

	if !runSupportsAction(sess.Kind, "signal") {
		writeProblemType(w, http.StatusConflict, "signal is not a supported action for this run", "unsupported-action")
		return
	}

	var req apiv1gen.SignalRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&req); err != nil && err != io.EOF {
		writeProblem(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if req.Name == "" {
		writeProblem(w, http.StatusBadRequest, "name is required")
		return
	}

	if s.deps.TemporalClient == nil {
		writeProblem(w, http.StatusServiceUnavailable, "temporal client not available")
		return
	}

	var arg any
	if req.Input != nil {
		arg = *req.Input
	}

	if err := s.deps.TemporalClient.SignalWorkflow(ctx, sess.WorkflowID, "", req.Name, arg); err != nil {
		s.logger.ErrorContext(ctx, "failed to signal workflow", "workflowId", sess.WorkflowID, "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to signal run")
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// messageFromStore converts a store.Message row to its API representation.
func messageFromStore(m *store.Message) apiv1gen.Message {
	id := m.ID.String()
	content := m.Content
	seq := m.Sequence
	createdAt := m.CreatedAt
	return apiv1gen.Message{
		Id:         id,
		Role:       m.Role,
		Content:    &content,
		Sequence:   &seq,
		CreateTime: &createdAt,
	}
}

// requireActiveRun loads the run and verifies it is active. Write handlers
// (send, retry) must reject non-active runs to avoid issuing Temporal updates
// on completed or expired workflows.
func (s *Server) requireActiveRun(w http.ResponseWriter, r *http.Request) *store.Session {
	sess := s.requireRun(w, r)
	if sess == nil {
		return nil
	}
	if sess.Status != store.SessionStatusActive {
		writeProblem(w, http.StatusConflict, "run is not active")
		return nil
	}
	return sess
}

// handleListRunMessages handles GET /api/v1/runs/{run}/messages.
func (s *Server) handleListRunMessages(w http.ResponseWriter, r *http.Request) {
	sess := s.requireRun(w, r)
	if sess == nil {
		return
	}
	ctx := r.Context()
	query := r.URL.Query()

	pageSize := parsePageSize(query.Get("pageSize"))
	offset, ok := decodePageToken(query.Get("pageToken"))
	if !ok {
		writeProblem(w, http.StatusBadRequest, "invalid page token")
		return
	}

	msgs, err := s.deps.Store.GetMessages(ctx, sess.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to get messages", "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to get messages")
		return
	}

	items := make([]apiv1gen.Message, 0, pageSize)
	var nextPageToken *string
	if offset < len(msgs) {
		end := offset + pageSize
		if end > len(msgs) {
			end = len(msgs)
		}
		for i := offset; i < end; i++ {
			items = append(items, messageFromStore(&msgs[i]))
		}
		if end < len(msgs) {
			tok := encodePageToken(end)
			nextPageToken = &tok
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(apiv1gen.MessageList{Items: items, NextPageToken: nextPageToken}); err != nil {
		s.logger.ErrorContext(ctx, "encode response", "error", err)
	}
}

// requireTextBodyV1 parses a {"content": "..."} body, emitting RFC 9457 problem
// responses on a bad or empty body so that malformed-body errors are
// application/problem+json.
func requireTextBodyV1(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&body); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid json body")
		return "", false
	}
	if body.Content == "" {
		writeProblem(w, http.StatusBadRequest, "content is required")
		return "", false
	}
	return body.Content, true
}

// handleSendRunMessage handles POST /api/v1/runs/{run}/messages.
func (s *Server) handleSendRunMessage(w http.ResponseWriter, r *http.Request) {
	sess := s.requireActiveRun(w, r)
	if sess == nil {
		return
	}
	if !runSupportsAction(sess.Kind, "messages") {
		writeProblemType(w, http.StatusConflict, "messages is not a supported action for this run", "unsupported-action")
		return
	}
	text, ok := requireTextBodyV1(w, r)
	if !ok {
		return
	}
	s.streamWorkflowTurn(w, r, sess, wf.UpdateSendMessage, wf.SendMessageRequest{Text: text})
}

// handleRetryRunMessage handles POST /api/v1/runs/{run}/messages/retry.
func (s *Server) handleRetryRunMessage(w http.ResponseWriter, r *http.Request) {
	sess := s.requireActiveRun(w, r)
	if sess == nil {
		return
	}
	if !runSupportsAction(sess.Kind, "messages/retry") {
		writeProblemType(w, http.StatusConflict, "messages/retry is not a supported action for this run", "unsupported-action")
		return
	}
	text, ok := requireTextBodyV1(w, r)
	if !ok {
		return
	}
	s.streamWorkflowTurn(w, r, sess, wf.UpdateRetryMessage, wf.RetryMessageRequest{Text: text})
}

// handleListRunApprovals handles GET /api/v1/runs/{run}/approvals. It returns
// the pending approvals for a run. Run kinds that do not support approvals
// (ticket, monitor) return an empty list rather than an error.
func (s *Server) handleListRunApprovals(w http.ResponseWriter, r *http.Request) {
	sess := s.requireRun(w, r)
	if sess == nil {
		return
	}
	ctx := r.Context()

	items := make([]apiv1gen.Approval, 0)

	switch sess.Kind {
	case store.SessionKindChat:
		if s.deps.TemporalClient == nil {
			writeProblem(w, http.StatusServiceUnavailable, "temporal client not available")
			return
		}
		resp, err := s.deps.TemporalClient.QueryWorkflow(ctx, sess.WorkflowID, "", wf.ChatApprovalsQuery)
		if err != nil {
			s.logger.ErrorContext(ctx, "failed to query approvals", "workflowId", sess.WorkflowID, "error", err)
			writeProblem(w, http.StatusInternalServerError, "failed to query approvals")
			return
		}
		var states []wf.ApprovalState
		if err := resp.Get(&states); err != nil {
			s.logger.ErrorContext(ctx, "failed to decode approvals", "workflowId", sess.WorkflowID, "error", err)
			writeProblem(w, http.StatusInternalServerError, "failed to decode approvals")
			return
		}
		for i := range states {
			a := &states[i]
			if a.Status != wf.ApprovalStatusPending {
				continue
			}
			items = append(items, apiv1gen.Approval{
				CallId:     a.CallID,
				ToolName:   ptr(a.Action),
				CreateTime: ptr(a.RequestedAt),
			})
		}
	case store.SessionKindAgentTask:
		if s.deps.Permits == nil {
			writeProblem(w, http.StatusServiceUnavailable, "approval store not available")
			return
		}
		for _, callID := range s.deps.Permits.PendingApprovals(sess.ID.String()) {
			id := callID
			items = append(items, apiv1gen.Approval{CallId: id})
		}
	default:
		// Other kinds (ticket, monitor) have no approvals; return empty list.
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(apiv1gen.ApprovalList{Items: items}); err != nil {
		s.logger.ErrorContext(ctx, "encode response", "error", err)
	}
}

// handleResolveRunApproval handles POST /api/v1/runs/{run}/approvals/{callId}/resolve.
func (s *Server) handleResolveRunApproval(w http.ResponseWriter, r *http.Request) {
	sess := s.requireRun(w, r)
	if sess == nil {
		return
	}
	ctx := r.Context()

	callID := r.PathValue("callId")
	if callID == "" {
		writeProblem(w, http.StatusBadRequest, "missing call id")
		return
	}

	if !runSupportsAction(sess.Kind, "approvals") {
		writeProblemType(w, http.StatusConflict, "resolving approvals is not a supported action for this run", "unsupported-action")
		return
	}

	var req apiv1gen.ResolveApprovalRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid json body")
		return
	}
	reason := ""
	if req.Reason != nil {
		reason = *req.Reason
	}

	var ok bool
	switch sess.Kind {
	case store.SessionKindChat:
		ok = s.resolveChatApproval(ctx, w, sess, callID, req.Approved, reason)
	case store.SessionKindAgentTask:
		ok = s.resolveAgentTaskApproval(ctx, w, sess, callID, req.Approved, reason)
	default:
		// Unreachable: the 409 gate above rejects kinds without "approvals".
		writeProblemType(w, http.StatusConflict, "resolving approvals is not a supported action for this run", "unsupported-action")
		return
	}
	if !ok {
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// resolveChatApproval resolves a pending approval for a chat run via a Temporal
// workflow update. It returns false (and writes a problem response) on failure.
func (s *Server) resolveChatApproval(ctx context.Context, w http.ResponseWriter, sess *store.Session, callID string, approved bool, reason string) bool {
	if s.deps.TemporalClient == nil {
		writeProblem(w, http.StatusServiceUnavailable, "temporal client not available")
		return false
	}
	approveCtx, approveCancel := context.WithTimeout(ctx, workflowUpdateTimeout)
	defer approveCancel()

	handle, err := s.deps.TemporalClient.UpdateWorkflow(approveCtx, client.UpdateWorkflowOptions{
		WorkflowID:   sess.WorkflowID,
		UpdateName:   wf.UpdateApproveAction,
		WaitForStage: client.WorkflowUpdateStageAccepted,
		Args: []any{wf.ApproveActionRequest{
			CallID:   callID,
			Approved: approved,
			Reason:   reason,
		}},
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "approval update failed",
			"runId", sess.ID.String(), "callId", callID, "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to send approval")
		return false
	}
	var resp wf.ApproveActionResponse
	if err := handle.Get(approveCtx, &resp); err != nil {
		s.logger.ErrorContext(ctx, "approval update get failed",
			"runId", sess.ID.String(), "callId", callID, "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to get approval result")
		return false
	}
	if resp.Status == wf.ApproveResponseConflict {
		writeProblem(w, http.StatusNotFound, "approval not found or already resolved")
		return false
	}
	return true
}

// resolveAgentTaskApproval resolves a pending approval for a fanout agent_task
// run via the in-process PermitStore. It returns false (and writes a problem
// response) on failure.
func (s *Server) resolveAgentTaskApproval(ctx context.Context, w http.ResponseWriter, sess *store.Session, callID string, approved bool, reason string) bool {
	if s.deps.Permits == nil {
		writeProblem(w, http.StatusServiceUnavailable, "approval store not available")
		return false
	}
	decision := decisionDeny
	if approved {
		decision = decisionAllow
	}
	runID := sess.ID.String()
	if !s.deps.Permits.ResolveApproval(runID, callID, ApprovalResolution{Decision: decision, Reason: reason}) {
		writeProblem(w, http.StatusNotFound, "no pending approval for this call")
		return false
	}
	if s.deps.ApprovalNotifier != nil {
		if err := s.deps.ApprovalNotifier.NotifyApprovalResolved(ctx, runID, callID, decision, reason); err != nil {
			s.logger.WarnContext(ctx, "failed to notify approval resolution",
				"runId", runID, "callId", callID, "error", err)
		}
	}
	return true
}

// handleListRunChildren handles GET /api/v1/runs/{run}/children. It returns the
// fanout child runs of a run. Non-fanout runs simply have no children.
func (s *Server) handleListRunChildren(w http.ResponseWriter, r *http.Request) {
	sess := s.requireRun(w, r)
	if sess == nil {
		return
	}
	ctx := r.Context()

	children, err := s.deps.Store.ListChildSessions(ctx, sess.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to list run children", "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to list run children")
		return
	}

	runs := make([]apiv1gen.Run, 0, len(children))
	for i := range children {
		runs = append(runs, runFromSession(children[i]))
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(apiv1gen.RunList{Items: runs}); err != nil {
		s.logger.ErrorContext(ctx, "encode response", "error", err)
	}
}

// handleRunGuidance handles POST /api/v1/runs/{run}/guidance. It injects
// mid-run operator guidance into a fanout agent_task run. This mirrors the
// legacy handlePostGuidance ordering and reasoning, but speaks RFC 9457
// problem+json and gates on the run kind. Body: GuidanceRequest.
func (s *Server) handleRunGuidance(w http.ResponseWriter, r *http.Request) {
	sess := s.requireRun(w, r)
	if sess == nil {
		return
	}
	ctx := r.Context()

	if !runSupportsAction(sess.Kind, "guidance") {
		writeProblemType(w, http.StatusConflict, "guidance is not a supported action for this run", "unsupported-action")
		return
	}

	if s.deps.GuidanceStore == nil {
		writeProblem(w, http.StatusServiceUnavailable, "guidance store not available")
		return
	}

	var req apiv1gen.GuidanceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if req.Text == "" {
		writeProblem(w, http.StatusBadRequest, "text is required")
		return
	}

	runID := sess.ID.String()
	if _, ok := s.deps.GuidanceStore.Receiver(runID); !ok {
		writeProblem(w, http.StatusNotFound, "no guidance channel for this run")
		return
	}

	bidi := s.deps.RunnerCaps != nil && s.deps.RunnerCaps.BidirectionalStream("", runID)
	if !bidi {
		writeProblemType(w, http.StatusConflict, "runner does not support mid-run guidance; use Stop with a new prompt", "guidance-unsupported")
		return
	}

	if s.deps.WorkflowUpdater == nil {
		writeProblem(w, http.StatusServiceUnavailable, "workflow updater not available")
		return
	}

	// Push first so a full or closed channel surfaces as 503 before the
	// workflow logs the entry to its durable state. Reversing this order would
	// leave the guidance log claiming "accepted" entries that never reached the
	// runner.
	if !s.deps.GuidanceStore.Push(runID, req.Text) {
		writeProblem(w, http.StatusServiceUnavailable, "guidance channel full or closed")
		return
	}

	if err := s.deps.WorkflowUpdater.InjectGuidance(ctx, runID, GuidanceUpdateArgs{
		Text:                          req.Text,
		RunnerCapsBidirectionalStream: bidi,
	}); err != nil {
		if errors.Is(err, ErrGuidanceUnsupported) {
			// Use the sentinel's message rather than err.Error(); the wrap chain
			// repeats the same phrasing twice.
			writeProblem(w, http.StatusConflict, ErrGuidanceUnsupported.Error())
			return
		}
		s.logger.ErrorContext(ctx, "inject guidance failed", "runId", runID, "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to inject guidance")
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// policyFromAPI maps an API ApprovalPolicy DTO to the backend
// SessionApprovalPolicy, dereferencing optional pointer fields safely (a nil
// pointer yields a false bool or a nil map).
func policyFromAPI(p apiv1gen.ApprovalPolicy) SessionApprovalPolicy {
	out := SessionApprovalPolicy{}
	if p.AutoApproveAll != nil {
		out.AutoApproveAll = *p.AutoApproveAll
	}
	if p.AutoApproveTools != nil {
		out.AutoApproveTools = *p.AutoApproveTools
	}
	if p.DenyTools != nil {
		out.DenyTools = *p.DenyTools
	}
	return out
}

// policyToAPI maps a backend SessionApprovalPolicy to its API DTO. The map
// fields are only set when non-empty so the encoded JSON omits empty objects.
func policyToAPI(p SessionApprovalPolicy) apiv1gen.ApprovalPolicy {
	out := apiv1gen.ApprovalPolicy{AutoApproveAll: ptr(p.AutoApproveAll)}
	if len(p.AutoApproveTools) > 0 {
		out.AutoApproveTools = ptr(p.AutoApproveTools)
	}
	if len(p.DenyTools) > 0 {
		out.DenyTools = ptr(p.DenyTools)
	}
	return out
}

// handleSetRunPolicy handles PUT /api/v1/runs/{run}/policy. It replaces the
// session approval policy for a fanout agent_task run. Body: ApprovalPolicy.
func (s *Server) handleSetRunPolicy(w http.ResponseWriter, r *http.Request) {
	sess := s.requireRun(w, r)
	if sess == nil {
		return
	}
	ctx := r.Context()

	if !runSupportsAction(sess.Kind, "policy") {
		writeProblemType(w, http.StatusConflict, "setting a policy is not a supported action for this run", "unsupported-action")
		return
	}

	if s.deps.Permits == nil {
		writeProblem(w, http.StatusServiceUnavailable, "permit store not available")
		return
	}

	var req apiv1gen.ApprovalPolicy
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid json body")
		return
	}

	runID := sess.ID.String()
	s.deps.Permits.SetSessionPolicy(runID, policyFromAPI(req))

	w.Header().Set("Content-Type", "application/json")
	if err := writeJSON(w, policyToAPI(s.deps.Permits.GetSessionPolicy(runID))); err != nil {
		s.logger.ErrorContext(ctx, "encode response", "error", err)
	}
}

// handleRunEvents handles GET /api/v1/runs/{run}/events.
//
// Authentication is via a short-lived single-use ?ticket= query parameter
// because browser EventSource cannot send an Authorization header. The ticket
// must be minted first via POST /api/v1/auth/sse-tickets (bearer-authed) and
// redeemed here within sseTicketTTL.
//
// The stream is resumable: on reconnect the client sends Last-Event-ID: N and
// only messages with sequence > N are replayed from the persisted log. Live
// broker events are intentionally written without an SSE id (via writeSSEEvent)
// so the client's Last-Event-ID always refers to a real persisted message
// sequence. Live events that get persisted will be replayed with their sequence
// id on the next reconnect; the client deduplicates by sequence.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	// Step 1: ticket auth. Must be first so pre-stream errors are returned as
	// problem+json (Content-Type not yet set to text/event-stream).
	tok := r.URL.Query().Get("ticket")
	if !s.tickets.redeem(tok) {
		writeProblem(w, http.StatusUnauthorized, "invalid or expired ticket")
		return
	}

	// Step 2: validate the run. requireRun writes a problem response on failure.
	sess := s.requireRun(w, r)
	if sess == nil {
		return
	}

	// Step 3: ensure the event broker is wired before committing to SSE mode, so
	// a missing dependency is a clean problem+json rather than a half-open stream.
	if s.deps.SessionBroker == nil {
		writeProblem(w, http.StatusServiceUnavailable, "event broker not available")
		return
	}

	// Step 4: verify streaming is supported before setting SSE headers.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Step 5: commit to SSE mode.
	setSSEHeaders(w)
	flusher.Flush()

	// Step 6: read resume cursor from reconnecting clients.
	resumeFrom := parseLastEventID(r)

	// Step 7: subscribe before backfill to avoid losing events emitted between
	// the DB query and the subscription call (race window fix).
	runIDStr := sess.ID.String()
	eventCh := s.deps.SessionBroker.Subscribe(runIDStr)
	defer s.deps.SessionBroker.Unsubscribe(runIDStr, eventCh)

	// Step 8: backfill from persisted log for durable resume.
	msgs, err := s.deps.Store.GetMessages(r.Context(), sess.ID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "backfill messages failed",
			"runId", runIDStr, "error", err)
		// Continue streaming live events even when backfill fails; the error
		// is transient and the client can reconnect with Last-Event-ID.
	} else {
		for i := range msgs {
			msg := &msgs[i]
			if msg.Sequence <= resumeFrom {
				continue
			}
			data, merr := json.Marshal(msg)
			if merr != nil {
				s.logger.ErrorContext(r.Context(), "backfill marshal failed",
					"runId", runIDStr, "sequence", msg.Sequence, "error", merr)
				continue
			}
			writeSSEFrame(w, msg.Sequence, "message", string(data))
			flusher.Flush()
		}
	}

	// Step 9: stream live events until the client disconnects or the broker closes.
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-eventCh:
			if !ok {
				return
			}
			writeSSEEvent(w, flusher, ev)
		}
	}
}

// handleExtendRunPolicy handles PATCH /api/v1/runs/{run}/policy. It applies an
// additive delta (auto-approve tools) to the session policy of a fanout
// agent_task run. Body: ApprovalPolicyDelta.
func (s *Server) handleExtendRunPolicy(w http.ResponseWriter, r *http.Request) {
	sess := s.requireRun(w, r)
	if sess == nil {
		return
	}
	ctx := r.Context()

	if !runSupportsAction(sess.Kind, "policy") {
		writeProblemType(w, http.StatusConflict, "extending a policy is not a supported action for this run", "unsupported-action")
		return
	}

	if s.deps.Permits == nil {
		writeProblem(w, http.StatusServiceUnavailable, "permit store not available")
		return
	}

	var req apiv1gen.ApprovalPolicyDelta
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid json body")
		return
	}

	delta := SessionApprovalPolicyDelta{}
	if req.AddTools != nil {
		delta.AddTools = *req.AddTools
	}

	runID := sess.ID.String()
	s.deps.Permits.ExtendSessionPolicy(runID, delta)

	w.Header().Set("Content-Type", "application/json")
	if err := writeJSON(w, policyToAPI(s.deps.Permits.GetSessionPolicy(runID))); err != nil {
		s.logger.ErrorContext(ctx, "encode response", "error", err)
	}
}
