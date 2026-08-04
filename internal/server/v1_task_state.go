package server

import (
	"net/http"

	"github.com/tphakala/alfred/internal/server/apiv1gen"
	"github.com/tphakala/alfred/internal/store"
)

// taskStateItemFromStore converts a store.TaskState row to its API
// representation. outcome is assigned directly as json.RawMessage so it
// round-trips byte-for-byte for any valid JSON value (object, array, string,
// number, boolean, or null); the engine still never interprets its contents.
func taskStateItemFromStore(ts *store.TaskState) apiv1gen.TaskStateItem {
	attempts := ts.Attempts
	createTime := ts.CreatedAt
	updateTime := ts.UpdatedAt

	item := apiv1gen.TaskStateItem{
		Task:         ts.Task,
		CandidateKey: ts.CandidateKey,
		Status:       ts.Status,
		Attempts:     &attempts,
		CreateTime:   &createTime,
		UpdateTime:   &updateTime,
	}
	if ts.CaseRunID != "" {
		item.CaseRunId = ptr(ts.CaseRunID)
	}
	if len(ts.Outcome) > 0 {
		item.Outcome = &ts.Outcome
	}
	return item
}

// handleListTaskState handles GET /api/v1/agent-tasks/{task}/state. It returns
// a page of the task_state ledger for the given task, newest updated first,
// optionally filtered by status. Paging is pushed into the store query.
func (s *Server) handleListTaskState(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeProblem(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	ctx := r.Context()
	task := r.PathValue("task")
	query := r.URL.Query()

	statusFilter := query.Get("status")
	pageSize := parsePageSize(query.Get("pageSize"))
	offset, ok := decodePageToken(query.Get("pageToken"))
	if !ok {
		writeProblem(w, http.StatusBadRequest, "invalid page token")
		return
	}

	// Fetch one extra row to detect whether a next page exists without a
	// separate count query.
	rows, err := s.deps.Store.ListTaskState(ctx, task, statusFilter, pageSize+1, offset)
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to list task state", "task", task, "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to list task state")
		return
	}

	items := make([]apiv1gen.TaskStateItem, 0, pageSize)
	var nextPageToken *string
	for i := range rows {
		if i >= pageSize {
			tok := encodePageToken(offset + pageSize)
			nextPageToken = &tok
			break
		}
		items = append(items, taskStateItemFromStore(&rows[i]))
	}

	w.Header().Set("Content-Type", "application/json")
	if err := writeJSON(w, apiv1gen.TaskStateList{Items: items, NextPageToken: nextPageToken}); err != nil {
		s.logger.ErrorContext(ctx, "encode response", "error", err)
	}
}

// handleRequeueTaskState handles POST /api/v1/agent-tasks/{task}/state/{key}/requeue.
// It clears the ledger row for this candidate so the next monitor pass
// treats it as new and re-dispatches it.
func (s *Server) handleRequeueTaskState(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeProblem(w, http.StatusServiceUnavailable, "store not available")
		return
	}
	ctx := r.Context()
	task := r.PathValue("task")
	key := r.PathValue("key")

	deleted, err := s.deps.Store.RequeueTaskState(ctx, task, key)
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to requeue task state", "task", task, "key", key, "error", err)
		writeProblem(w, http.StatusInternalServerError, "failed to requeue task state")
		return
	}
	if !deleted {
		writeProblem(w, http.StatusNotFound, "no task state for this task and key")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
