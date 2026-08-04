package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// EpochTransition represents a row in the epoch_transitions audit-log table.
type EpochTransition struct {
	ID             uuid.UUID
	FromSession    uuid.UUID
	ToSession      uuid.UUID
	Trigger        string
	ExtractedFacts string
	Summary        string
	CreatedAt      time.Time
}

// CreateEpochTransition inserts a transition record.
// It populates tr.CreatedAt from the database-generated value via RETURNING.
func (s *Store) CreateEpochTransition(ctx context.Context, tr *EpochTransition) error {
	const q = `
		INSERT INTO epoch_transitions
			(id, from_session, to_session, trigger, extracted_facts, summary)
		VALUES
			($1, $2, $3, $4, $5, $6)
		RETURNING created_at`

	return s.pool.QueryRow(ctx, q,
		tr.ID,
		tr.FromSession,
		tr.ToSession,
		tr.Trigger,
		nilIfEmpty(tr.ExtractedFacts),
		nilIfEmpty(tr.Summary),
	).Scan(&tr.CreatedAt)
}

// GetEpochTransitions returns all transitions originating from fromSessionID,
// ordered by created_at ascending.
func (s *Store) GetEpochTransitions(ctx context.Context, fromSessionID uuid.UUID) ([]EpochTransition, error) {
	const q = `
		SELECT id, from_session, to_session, trigger, extracted_facts, summary, created_at
		FROM epoch_transitions
		WHERE from_session = $1
		ORDER BY created_at ASC`

	rows, err := s.pool.Query(ctx, q, fromSessionID)
	if err != nil {
		return nil, fmt.Errorf("get epoch transitions for session %s: %w", fromSessionID, err)
	}
	defer rows.Close()

	var transitions []EpochTransition
	for rows.Next() {
		var tr EpochTransition
		var extractedFacts *string
		var summary *string

		if err := rows.Scan(
			&tr.ID,
			&tr.FromSession,
			&tr.ToSession,
			&tr.Trigger,
			&extractedFacts,
			&summary,
			&tr.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan epoch transition row: %w", err)
		}

		if extractedFacts != nil {
			tr.ExtractedFacts = *extractedFacts
		}
		if summary != nil {
			tr.Summary = *summary
		}

		transitions = append(transitions, tr)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate epoch transition rows: %w", err)
	}

	return transitions, nil
}
