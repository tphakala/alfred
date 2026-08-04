package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Session status constants.
const (
	SessionStatusActive    = "active"
	SessionStatusCompleted = "completed"
	SessionStatusContinued = "continued"
)

// Session kind constants.
const (
	SessionKindChat      = "chat"
	SessionKindTicket    = "ticket"
	SessionKindMonitor   = "monitor"
	SessionKindAgentTask = "agent_task"
	SessionKindCase      = "case"
)

// listSessionsLimit is the maximum number of sessions returned by ListSessions.
const listSessionsLimit = 100

// Session represents a workflow session stored in the sessions table.
type Session struct {
	ID            uuid.UUID  `json:"id"`
	WorkflowID    string     `json:"workflowId"`
	RunID         string     `json:"runId"`
	ParentSession *uuid.UUID `json:"parentSession,omitempty"`
	Kind          string     `json:"kind"`
	Status        string     `json:"status"`
	Epoch         int        `json:"epoch"`
	EpochSummary  string     `json:"epochSummary,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// nilIfEmpty converts an empty string to nil, suitable for nullable TEXT columns.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// scanSession scans a single session row from a pgx.Row or pgx.Rows scanner.
func scanSession(row interface {
	Scan(dest ...any) error
}) (*Session, error) {
	sess := &Session{}
	var runID *string
	var epochSummary *string

	if err := row.Scan(
		&sess.ID,
		&sess.WorkflowID,
		&runID,
		&sess.ParentSession,
		&sess.Kind,
		&sess.Status,
		&sess.Epoch,
		&epochSummary,
		&sess.CreatedAt,
		&sess.UpdatedAt,
	); err != nil {
		return nil, err
	}

	if runID != nil {
		sess.RunID = *runID
	}
	if epochSummary != nil {
		sess.EpochSummary = *epochSummary
	}

	return sess, nil
}

// CreateSession inserts a new session row. It populates sess.CreatedAt and
// sess.UpdatedAt from the database-generated values via RETURNING.
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	const q = `
		INSERT INTO sessions
			(id, workflow_id, run_id, parent_session, kind, status, epoch, epoch_summary)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at, updated_at`

	epoch := sess.Epoch
	if epoch == 0 {
		epoch = 1
	}

	return s.pool.QueryRow(ctx, q,
		sess.ID,
		sess.WorkflowID,
		nilIfEmpty(sess.RunID),
		sess.ParentSession, // already *uuid.UUID, nil if not set
		sess.Kind,
		sess.Status,
		epoch,
		nilIfEmpty(sess.EpochSummary),
	).Scan(&sess.CreatedAt, &sess.UpdatedAt)
}

// GetSession retrieves a session by primary key.
// Returns a wrapped pgx.ErrNoRows if the session does not exist.
func (s *Store) GetSession(ctx context.Context, id uuid.UUID) (*Session, error) {
	const q = `
		SELECT id, workflow_id, run_id, parent_session, kind, status,
		       epoch, epoch_summary, created_at, updated_at
		FROM sessions
		WHERE id = $1`

	sess, err := scanSession(s.pool.QueryRow(ctx, q, id))
	if err != nil {
		return nil, fmt.Errorf("get session %s: %w", id, err)
	}

	return sess, nil
}

// GetSessionByWorkflowID retrieves the most recent session for a given workflow ID.
// Returns a wrapped pgx.ErrNoRows if no session exists for that workflow.
func (s *Store) GetSessionByWorkflowID(ctx context.Context, workflowID string) (*Session, error) {
	const q = `
		SELECT id, workflow_id, run_id, parent_session, kind, status,
		       epoch, epoch_summary, created_at, updated_at
		FROM sessions
		WHERE workflow_id = $1
		ORDER BY created_at DESC
		LIMIT 1`

	sess, err := scanSession(s.pool.QueryRow(ctx, q, workflowID))
	if err != nil {
		return nil, fmt.Errorf("get session by workflow %s: %w", workflowID, err)
	}

	return sess, nil
}

// UpdateSessionStatus sets the status and updated_at of a session.
// Returns a wrapped pgx.ErrNoRows if the session does not exist.
func (s *Store) UpdateSessionStatus(ctx context.Context, id uuid.UUID, status string) error {
	const q = `
		UPDATE sessions
		SET status = $2, updated_at = now()
		WHERE id = $1`

	tag, err := s.pool.Exec(ctx, q, id, status)
	if err != nil {
		return fmt.Errorf("update session status %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update session status %s: %w", id, pgx.ErrNoRows)
	}
	return nil
}

// UpdateSessionRunID sets the run_id and updated_at of a session.
// Returns a wrapped pgx.ErrNoRows if the session does not exist.
func (s *Store) UpdateSessionRunID(ctx context.Context, id uuid.UUID, runID string) error {
	const q = `
		UPDATE sessions
		SET run_id = $2, updated_at = now()
		WHERE id = $1`

	tag, err := s.pool.Exec(ctx, q, id, nilIfEmpty(runID))
	if err != nil {
		return fmt.Errorf("update session run_id %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update session run_id %s: %w", id, pgx.ErrNoRows)
	}
	return nil
}

// UpdateSessionEpoch sets the epoch number and epoch_summary of a session.
// Used during epoch transitions before Continue-As-New.
func (s *Store) UpdateSessionEpoch(ctx context.Context, id uuid.UUID, epoch int, epochSummary string) error {
	const q = `
		UPDATE sessions
		SET epoch = $2, epoch_summary = $3, updated_at = now()
		WHERE id = $1`

	tag, err := s.pool.Exec(ctx, q, id, epoch, nilIfEmpty(epochSummary))
	if err != nil {
		return fmt.Errorf("update session epoch %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update session epoch %s: %w", id, pgx.ErrNoRows)
	}
	return nil
}

// ListChildSessions returns the sessions whose parent_session is the given id,
// ordered by creation time. Used to list fanout child runs.
func (s *Store) ListChildSessions(ctx context.Context, parentID uuid.UUID) ([]Session, error) {
	const q = `
		SELECT id, workflow_id, run_id, parent_session, kind, status,
		       epoch, epoch_summary, created_at, updated_at
		FROM sessions
		WHERE parent_session = $1
		ORDER BY created_at ASC
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, parentID, listSessionsLimit)
	if err != nil {
		return nil, fmt.Errorf("list child sessions of %s: %w", parentID, err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan child session: %w", err)
		}
		out = append(out, *sess)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate child sessions: %w", err)
	}

	return out, nil
}

// ListSessions returns sessions with the given status, newest first,
// capped at listSessionsLimit rows.
func (s *Store) ListSessions(ctx context.Context, status string) ([]Session, error) {
	const q = `
		SELECT id, workflow_id, run_id, parent_session, kind, status,
		       epoch, epoch_summary, created_at, updated_at
		FROM sessions
		WHERE status = $1
		ORDER BY created_at DESC
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, status, listSessionsLimit)
	if err != nil {
		return nil, fmt.Errorf("list sessions by status %q: %w", status, err)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session row: %w", err)
		}
		sessions = append(sessions, *sess)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session rows: %w", err)
	}

	return sessions, nil
}
