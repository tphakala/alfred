package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Message represents a single message in a session's conversation history.
type Message struct {
	ID             uuid.UUID       `json:"id"`
	SessionID      uuid.UUID       `json:"sessionId"`
	Sequence       int             `json:"sequence"`
	Role           string          `json:"role"`
	Content        string          `json:"content"`
	TokenEstimate  int             `json:"tokenEstimate"`
	Metadata       json.RawMessage `json:"metadata"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
}

// AppendMessages inserts messages in a single transaction.
// Uses ON CONFLICT (session_id, sequence) DO NOTHING for idempotent retries.
func (s *Store) AppendMessages(ctx context.Context, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // intentional rollback on early return; error discarded

	if err := insertMessagesInTx(ctx, tx, msgs); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit messages: %w", err)
	}
	return nil
}

const messageInsertQ = `
	INSERT INTO messages
		(id, session_id, sequence, role, content, token_estimate, metadata)
	VALUES
		($1, $2, $3, $4, $5, $6, $7)
	ON CONFLICT (session_id, sequence) DO NOTHING`

func insertMessagesInTx(ctx context.Context, tx pgx.Tx, msgs []Message) error {
	batch := &pgx.Batch{}
	for i := range msgs {
		msg := &msgs[i]
		var meta any
		if len(msg.Metadata) > 0 {
			meta = []byte(msg.Metadata)
		}
		batch.Queue(messageInsertQ,
			msg.ID,
			msg.SessionID,
			msg.Sequence,
			msg.Role,
			msg.Content,
			msg.TokenEstimate,
			meta,
		)
	}
	br := tx.SendBatch(ctx, batch)
	var batchErr error
	for i := range msgs {
		if _, err := br.Exec(); err != nil {
			batchErr = fmt.Errorf("insert message seq %d: %w", msgs[i].Sequence, err)
			break
		}
	}
	if err := br.Close(); err != nil && batchErr == nil {
		batchErr = fmt.Errorf("close batch: %w", err)
	}
	return batchErr
}

// GetMessages loads all messages for a session ordered by sequence ASC.
func (s *Store) GetMessages(ctx context.Context, sessionID uuid.UUID) ([]Message, error) {
	const q = `
		SELECT id, session_id, sequence, role, content,
		       COALESCE(token_estimate, 0), metadata, created_at
		FROM messages
		WHERE session_id = $1
		ORDER BY sequence ASC`

	rows, err := s.pool.Query(ctx, q, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query messages for session %s: %w", sessionID, err)
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var msg Message
		var meta []byte
		if err := rows.Scan(
			&msg.ID,
			&msg.SessionID,
			&msg.Sequence,
			&msg.Role,
			&msg.Content,
			&msg.TokenEstimate,
			&meta,
			&msg.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan message row: %w", err)
		}
		if len(meta) > 0 {
			msg.Metadata = json.RawMessage(meta)
		}
		msgs = append(msgs, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate message rows: %w", err)
	}

	return msgs, nil
}

// GetSessionTokenCount returns the sum of token_estimate for all messages in a session.
// Returns 0 if the session has no messages.
func (s *Store) GetSessionTokenCount(ctx context.Context, sessionID uuid.UUID) (int, error) {
	const q = `SELECT COALESCE(SUM(token_estimate), 0) FROM messages WHERE session_id = $1`

	var total int
	if err := s.pool.QueryRow(ctx, q, sessionID).Scan(&total); err != nil {
		return 0, fmt.Errorf("get token count for session %s: %w", sessionID, err)
	}
	return total, nil
}

// GetMessageCount returns the number of messages in a session.
func (s *Store) GetMessageCount(ctx context.Context, sessionID uuid.UUID) (int, error) {
	const q = `SELECT COUNT(*) FROM messages WHERE session_id = $1`

	var count int
	if err := s.pool.QueryRow(ctx, q, sessionID).Scan(&count); err != nil {
		return 0, fmt.Errorf("get message count for session %s: %w", sessionID, err)
	}
	return count, nil
}

// GetNextSequence returns MAX(sequence)+1 for a session, or 1 if the session has no messages.
func (s *Store) GetNextSequence(ctx context.Context, sessionID uuid.UUID) (int, error) {
	const q = `SELECT COALESCE(MAX(sequence), 0) + 1 FROM messages WHERE session_id = $1`

	var next int
	if err := s.pool.QueryRow(ctx, q, sessionID).Scan(&next); err != nil {
		return 0, fmt.Errorf("get next sequence for session %s: %w", sessionID, err)
	}
	return next, nil
}

// AppendMessagesAutoSeq allocates sequence numbers and inserts messages in a
// single transaction. It locks the session row with FOR UPDATE to serialize
// concurrent inserts and prevent duplicate sequences.
func (s *Store) AppendMessagesAutoSeq(ctx context.Context, sessionID uuid.UUID, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // intentional rollback on early return; error discarded

	// Lock the session row to prevent concurrent inserts from producing duplicate sequences.
	var locked int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sessionID).Scan(&locked); err != nil {
		return fmt.Errorf("lock session %s: %w", sessionID, err)
	}
	const seqQ = `SELECT COALESCE(MAX(sequence), 0) + 1 FROM messages WHERE session_id = $1`
	var nextSeq int
	if err := tx.QueryRow(ctx, seqQ, sessionID).Scan(&nextSeq); err != nil {
		return fmt.Errorf("get next sequence for session %s: %w", sessionID, err)
	}

	for i := range msgs {
		msgs[i].Sequence = nextSeq + i
		msgs[i].SessionID = sessionID
	}
	if err := insertMessagesInTx(ctx, tx, msgs); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit messages: %w", err)
	}
	return nil
}

const messageInsertIdempotentQ = `
	INSERT INTO messages
		(id, session_id, sequence, role, content, token_estimate, metadata, idempotency_key)
	VALUES
		($1, $2, $3, $4, $5, $6, $7, $8)
	ON CONFLICT (session_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`

// AppendMessagesIdempotent allocates sequence numbers and inserts messages in a
// single transaction with idempotency-key-based deduplication. It locks the
// session row with FOR UPDATE to serialize concurrent inserts. Messages whose
// idempotency key already exists for the session are silently skipped via
// ON CONFLICT DO NOTHING, making this safe for Temporal activity retries.
func (s *Store) AppendMessagesIdempotent(ctx context.Context, sessionID uuid.UUID, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // intentional rollback on early return

	// Lock session row to serialize concurrent inserts.
	var locked int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sessionID).Scan(&locked); err != nil {
		return fmt.Errorf("lock session %s: %w", sessionID, err)
	}
	const seqQ = `SELECT COALESCE(MAX(sequence), 0) + 1 FROM messages WHERE session_id = $1`
	var nextSeq int
	if err := tx.QueryRow(ctx, seqQ, sessionID).Scan(&nextSeq); err != nil {
		return fmt.Errorf("get next sequence for session %s: %w", sessionID, err)
	}

	batch := &pgx.Batch{}
	for i := range msgs {
		msgs[i].Sequence = nextSeq + i
		msgs[i].SessionID = sessionID
		var meta any
		if len(msgs[i].Metadata) > 0 {
			meta = []byte(msgs[i].Metadata)
		}
		var idempKey any
		if msgs[i].IdempotencyKey != "" {
			idempKey = msgs[i].IdempotencyKey
		}
		batch.Queue(messageInsertIdempotentQ,
			msgs[i].ID,
			msgs[i].SessionID,
			msgs[i].Sequence,
			msgs[i].Role,
			msgs[i].Content,
			msgs[i].TokenEstimate,
			meta,
			idempKey,
		)
	}
	br := tx.SendBatch(ctx, batch)
	var batchErr error
	for i := range msgs {
		if _, err := br.Exec(); err != nil {
			batchErr = fmt.Errorf("insert message seq %d: %w", msgs[i].Sequence, err)
			break
		}
	}
	if err := br.Close(); err != nil && batchErr == nil {
		batchErr = fmt.Errorf("close batch: %w", err)
	}
	if batchErr != nil {
		return batchErr
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit messages: %w", err)
	}
	return nil
}

// DeleteMessagesAfterSequence removes all messages with sequence > afterSequence
// for the given session. It locks the session row with FOR UPDATE to serialize
// with concurrent AppendMessagesAutoSeq calls. Returns the number of rows deleted.
func (s *Store) DeleteMessagesAfterSequence(ctx context.Context, sessionID uuid.UUID, afterSequence int) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // intentional rollback on early return; error discarded

	var locked int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sessionID).Scan(&locked); err != nil {
		return 0, fmt.Errorf("lock session %s: %w", sessionID, err)
	}

	tag, err := tx.Exec(ctx, `DELETE FROM messages WHERE session_id = $1 AND sequence > $2`, sessionID, afterSequence)
	if err != nil {
		return 0, fmt.Errorf("delete messages after seq %d for session %s: %w", afterSequence, sessionID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit delete: %w", err)
	}
	return tag.RowsAffected(), nil
}
