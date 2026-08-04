package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/tphakala/alfred/internal/store"
)

// MonitorStore is the subset of store.Store needed by the queue monitor and
// case activities: the task_state ledger DAL plus session creation for a
// case's claim step.
type MonitorStore interface {
	GetTaskState(ctx context.Context, task, candidateKey string) (*store.TaskState, error)
	UpsertClaim(ctx context.Context, task, candidateKey, caseRunID string) (int, error)
	FinalizeOutcome(ctx context.Context, task, candidateKey, status string, outcome json.RawMessage) error
	CountInProgress(ctx context.Context, task string) (int, error)
	CreateSession(ctx context.Context, sess *store.Session) error
}

// MonitorActivities is the struct-based activity bundle for the
// queue_monitor workflow and the case workflow it dispatches. All
// task_state ledger reads and writes happen here, inside activities, never
// in workflow code (Temporal determinism).
type MonitorActivities struct {
	Store MonitorStore
}

// DedupResult is the per-key dispatch decision returned by DedupCandidates.
type DedupResult struct {
	Dispatch []string `json:"dispatch,omitempty"`
	Skip     []string `json:"skip,omitempty"`
	Held     []string `json:"held,omitempty"`
}

// DedupCandidates classifies each candidate key against the task_state
// ledger: Dispatch (no row, or a non-terminal row under the attempt cap),
// Skip (a terminal row), or Held (a non-terminal row at or over the attempt
// cap). A held candidate is finalized needs_attention by this activity
// itself, so the workflow never writes the ledger directly.
func (a *MonitorActivities) DedupCandidates(ctx context.Context, task string, keys []string, maxCaseAttempts int) (DedupResult, error) {
	if a.Store == nil {
		return DedupResult{}, fmt.Errorf("store not configured")
	}

	var result DedupResult
	for _, key := range keys {
		ts, err := a.Store.GetTaskState(ctx, task, key)
		if err != nil {
			return DedupResult{}, fmt.Errorf("dedup candidate %s/%s: %w", task, key, err)
		}
		switch {
		case ts == nil:
			result.Dispatch = append(result.Dispatch, key)
		case isTerminalTaskStatus(ts.Status):
			result.Skip = append(result.Skip, key)
		case ts.Attempts >= maxCaseAttempts:
			if err := a.Store.FinalizeOutcome(ctx, task, key, store.TaskStatusNeedsAttention, nil); err != nil {
				return DedupResult{}, fmt.Errorf("hold candidate %s/%s: %w", task, key, err)
			}
			result.Held = append(result.Held, key)
		default:
			result.Dispatch = append(result.Dispatch, key)
		}
	}
	return result, nil
}

// isTerminalTaskStatus delegates to store.IsTerminalTaskStatus, the single
// source for the terminal status set (see task_state.go).
func isTerminalTaskStatus(status string) bool {
	return store.IsTerminalTaskStatus(status)
}

// CountInProgress returns the number of in_progress task_state rows for a
// task, used by the workflow to enforce max_parallel_cases.
func (a *MonitorActivities) CountInProgress(ctx context.Context, task string) (int, error) {
	if a.Store == nil {
		return 0, fmt.Errorf("store not configured")
	}
	return a.Store.CountInProgress(ctx, task)
}

// ClaimCaseArgs is the input to ClaimCase.
type ClaimCaseArgs struct {
	Task         string
	CandidateKey string
	SessionID    uuid.UUID
	WorkflowID   string
	RunID        string
}

// ClaimCase upserts the task_state ledger row as in_progress (incrementing
// attempts) and creates the case's session row, in that order. This runs as
// the case workflow's first durable step, so a ledger claim exists only if a
// case actually started, and GET /api/v1/runs/{id} does not 404 for a
// running case.
func (a *MonitorActivities) ClaimCase(ctx context.Context, args ClaimCaseArgs) error { //nolint:gocritic // hugeParam: activity params are value-semantic for Temporal serialization
	if a.Store == nil {
		return fmt.Errorf("store not configured")
	}
	if _, err := a.Store.UpsertClaim(ctx, args.Task, args.CandidateKey, args.RunID); err != nil {
		return fmt.Errorf("claim case %s/%s: %w", args.Task, args.CandidateKey, err)
	}
	if err := a.Store.CreateSession(ctx, &store.Session{
		ID:         args.SessionID,
		WorkflowID: args.WorkflowID,
		RunID:      args.RunID,
		Kind:       store.SessionKindCase,
		Status:     store.SessionStatusActive,
		Epoch:      1,
	}); err != nil && !store.IsConflict(err) {
		return fmt.Errorf("create case session: %w", err)
	}
	return nil
}

// FinalizeCaseArgs is the input to FinalizeCase.
type FinalizeCaseArgs struct {
	Task         string
	CandidateKey string
	Status       string
	Outcome      json.RawMessage
}

// FinalizeCase writes the case's terminal outcome to the task_state ledger.
func (a *MonitorActivities) FinalizeCase(ctx context.Context, args FinalizeCaseArgs) error {
	if a.Store == nil {
		return fmt.Errorf("store not configured")
	}
	if err := a.Store.FinalizeOutcome(ctx, args.Task, args.CandidateKey, args.Status, args.Outcome); err != nil {
		return fmt.Errorf("finalize case %s/%s: %w", args.Task, args.CandidateKey, err)
	}
	return nil
}
