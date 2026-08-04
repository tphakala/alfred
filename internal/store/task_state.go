package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// Task state status constants.
const (
	TaskStatusInProgress     = "in_progress"
	TaskStatusDone           = "done"
	TaskStatusSkipped        = "skipped"
	TaskStatusEscalated      = "escalated"
	TaskStatusNeedsAttention = "needs_attention"
)

// terminalTaskStatuses is the single source for which task_state statuses are
// terminal: no further case is dispatched for a (task, candidate_key) once
// its row reaches one of these. Both the queue monitor's dedup check
// (isTerminalTaskStatus) and the case supervisor's record_outcome tool
// schema (recordOutcomeStatuses) derive from this slice so they cannot drift
// apart.
var terminalTaskStatuses = []string{
	TaskStatusDone,
	TaskStatusSkipped,
	TaskStatusEscalated,
	TaskStatusNeedsAttention,
}

// IsTerminalTaskStatus reports whether status is one of the terminal
// task_state statuses.
func IsTerminalTaskStatus(status string) bool {
	return slices.Contains(terminalTaskStatuses, status)
}

// TerminalTaskStatuses returns a freshly allocated copy of the terminal
// task_state statuses, so callers cannot mutate the shared slice.
func TerminalTaskStatuses() []string {
	out := make([]string, len(terminalTaskStatuses))
	copy(out, terminalTaskStatuses)
	return out
}

// TaskState represents a single row in the task_state ledger: the durable
// cross-run record of which candidates a task has handled and their outcome.
// task and candidate_key are opaque strings defined by the deployment; the
// engine never interprets outcome.
type TaskState struct {
	Task         string
	CandidateKey string
	Status       string
	Outcome      json.RawMessage // nullable jsonb
	CaseRunID    string          // nullable text ("" when NULL)
	Attempts     int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// scanTaskState scans a single task_state row from a pgx.Row or pgx.Rows scanner.
func scanTaskState(row interface {
	Scan(dest ...any) error
}) (*TaskState, error) {
	ts := &TaskState{}
	var outcome []byte
	var caseRunID *string

	if err := row.Scan(
		&ts.Task,
		&ts.CandidateKey,
		&ts.Status,
		&outcome,
		&caseRunID,
		&ts.Attempts,
		&ts.CreatedAt,
		&ts.UpdatedAt,
	); err != nil {
		return nil, err
	}

	if len(outcome) > 0 {
		ts.Outcome = json.RawMessage(outcome)
	}
	if caseRunID != nil {
		ts.CaseRunID = *caseRunID
	}

	return ts, nil
}

// UpsertClaim inserts the (task, candidateKey) row as in_progress with
// attempts=1, or on conflict sets status=in_progress, updates case_run_id,
// and bumps updated_at. attempts is incremented only when the incoming
// caseRunID differs from the row's existing case_run_id, so a Temporal
// activity retry that re-calls UpsertClaim with the same run id (because a
// later step in the same attempt failed) leaves attempts unchanged; only a
// genuinely new dispatch counts against the attempt cap. Returns the new
// attempts value. This is how a Case workflow claims its candidate.
func (s *Store) UpsertClaim(ctx context.Context, task, candidateKey, caseRunID string) (int, error) {
	const q = `
		INSERT INTO task_state (task, candidate_key, status, case_run_id, attempts)
		VALUES ($1, $2, 'in_progress', $3, 1)
		ON CONFLICT (task, candidate_key) DO UPDATE SET
			status = 'in_progress',
			case_run_id = EXCLUDED.case_run_id,
			attempts = task_state.attempts + CASE
				WHEN task_state.case_run_id IS DISTINCT FROM EXCLUDED.case_run_id THEN 1
				ELSE 0
			END,
			updated_at = now()
		RETURNING attempts`

	var attempts int
	if err := s.pool.QueryRow(ctx, q, task, candidateKey, nilIfEmpty(caseRunID)).Scan(&attempts); err != nil {
		return 0, fmt.Errorf("upsert claim %s/%s: %w", task, candidateKey, err)
	}
	return attempts, nil
}

// FinalizeOutcome sets a terminal status ('done'|'skipped'|'escalated'|'needs_attention')
// and the outcome JSON for an existing (task, candidateKey) row, bumping
// updated_at. Returns an error if no row exists.
func (s *Store) FinalizeOutcome(ctx context.Context, task, candidateKey, status string, outcome json.RawMessage) error {
	const q = `
		UPDATE task_state
		SET status = $3, outcome = $4, updated_at = now()
		WHERE task = $1 AND candidate_key = $2`

	var outcomeArg any
	if len(outcome) > 0 {
		outcomeArg = []byte(outcome)
	}

	tag, err := s.pool.Exec(ctx, q, task, candidateKey, status, outcomeArg)
	if err != nil {
		return fmt.Errorf("finalize outcome %s/%s: %w", task, candidateKey, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("finalize outcome %s/%s: no such task state row", task, candidateKey)
	}
	return nil
}

// GetTaskState returns the row for (task, candidateKey), or (nil, nil) if absent.
func (s *Store) GetTaskState(ctx context.Context, task, candidateKey string) (*TaskState, error) {
	const q = `
		SELECT task, candidate_key, status, outcome, case_run_id, attempts, created_at, updated_at
		FROM task_state
		WHERE task = $1 AND candidate_key = $2`

	ts, err := scanTaskState(s.pool.QueryRow(ctx, q, task, candidateKey))
	if err != nil {
		if IsNotFound(err) {
			return nil, nil //nolint:nilnil // absent row is a valid, non-error result per the DAL contract
		}
		return nil, fmt.Errorf("get task state %s/%s: %w", task, candidateKey, err)
	}
	return ts, nil
}

// ListTaskState returns rows for a task ordered by updated_at DESC, optionally
// filtered by status (statusFilter == "" means all statuses), with SQL
// LIMIT/OFFSET paging pushed into the query.
func (s *Store) ListTaskState(ctx context.Context, task, statusFilter string, limit, offset int) ([]TaskState, error) {
	const q = `
		SELECT task, candidate_key, status, outcome, case_run_id, attempts, created_at, updated_at
		FROM task_state
		WHERE task = $1 AND ($2 = '' OR status = $2)
		ORDER BY updated_at DESC
		LIMIT $3 OFFSET $4`

	rows, err := s.pool.Query(ctx, q, task, statusFilter, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list task state for %s: %w", task, err)
	}
	defer rows.Close()

	var out []TaskState
	for rows.Next() {
		ts, scanErr := scanTaskState(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan task state row: %w", scanErr)
		}
		out = append(out, *ts)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task state rows: %w", err)
	}

	return out, nil
}

// CountInProgress returns the number of in_progress rows for a task (used
// later for concurrency caps).
func (s *Store) CountInProgress(ctx context.Context, task string) (int, error) {
	const q = `SELECT COUNT(*) FROM task_state WHERE task = $1 AND status = $2`

	var count int
	if err := s.pool.QueryRow(ctx, q, task, TaskStatusInProgress).Scan(&count); err != nil {
		return 0, fmt.Errorf("count in_progress for %s: %w", task, err)
	}
	return count, nil
}

// RequeueTaskState deletes the ledger row for (task, candidateKey) so the next
// monitor pass treats the candidate as new and re-dispatches it. Returns true
// if a row was deleted, false if none existed. Deleting the row is the chosen
// implementation of "clear terminal status and reset attempts": from the
// dedup query's perspective, a missing row and a reset row are identical, and
// delete is simpler and race-free.
func (s *Store) RequeueTaskState(ctx context.Context, task, candidateKey string) (bool, error) {
	const q = `DELETE FROM task_state WHERE task = $1 AND candidate_key = $2`

	tag, err := s.pool.Exec(ctx, q, task, candidateKey)
	if err != nil {
		return false, fmt.Errorf("requeue task state %s/%s: %w", task, candidateKey, err)
	}
	return tag.RowsAffected() > 0, nil
}
