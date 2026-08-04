package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/tphakala/alfred/internal/store"
)

const (
	testKindChat      = "chat"
	testStatusActive  = "active"
	testRoleAssistant = "assistant"
)

func newTestStore(t *testing.T) (s *store.Store, connStr string) {
	t.Helper()
	ctx := t.Context()

	pgC, err := postgres.Run(ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("alfred_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { pgC.Terminate(context.WithoutCancel(t.Context())) }) //nolint:errcheck // container teardown error is non-actionable in tests

	var err2 error
	connStr, err2 = pgC.ConnectionString(ctx, "sslmode=disable")
	if err2 != nil {
		t.Fatalf("connection string: %v", err2)
	}

	s, err2 = store.New(ctx, connStr, 5)
	if err2 != nil {
		t.Fatalf("new store: %v", err2)
	}
	t.Cleanup(s.Close)

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return s, connStr
}

func TestStore_ConnectAndMigrate(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}
	_, _ = newTestStore(t)
}

func TestStore_SessionCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}

	s, _ := newTestStore(t)
	ctx := t.Context()

	t.Run("CreateAndGetByID", func(t *testing.T) {
		sess := &store.Session{
			ID:         uuid.New(),
			WorkflowID: "wf-test-1",
			RunID:      "run-abc",
			Kind:       testKindChat,
			Status:     testStatusActive,
		}

		err := s.CreateSession(ctx, sess)
		require.NoError(t, err)
		assert.False(t, sess.CreatedAt.IsZero(), "CreatedAt should be set from RETURNING")
		assert.False(t, sess.UpdatedAt.IsZero(), "UpdatedAt should be set from RETURNING")

		got, err := s.GetSession(ctx, sess.ID)
		require.NoError(t, err)
		assert.Equal(t, sess.ID, got.ID)
		assert.Equal(t, sess.WorkflowID, got.WorkflowID)
		assert.Equal(t, sess.RunID, got.RunID)
		assert.Equal(t, sess.Kind, got.Kind)
		assert.Equal(t, sess.Status, got.Status)
		assert.Equal(t, 1, got.Epoch)
		assert.WithinDuration(t, sess.CreatedAt, got.CreatedAt, time.Second)
	})

	t.Run("GetByID_NotFound", func(t *testing.T) {
		_, err := s.GetSession(ctx, uuid.New())
		require.Error(t, err)
		assert.ErrorIs(t, err, pgx.ErrNoRows)
	})

	t.Run("GetByWorkflowID", func(t *testing.T) {
		wfID := "wf-test-workflow-" + uuid.New().String()

		sess1 := &store.Session{
			ID:         uuid.New(),
			WorkflowID: wfID,
			Kind:       "ticket",
			Status:     "completed",
		}
		require.NoError(t, s.CreateSession(ctx, sess1))

		// Small sleep to ensure distinct created_at ordering
		time.Sleep(5 * time.Millisecond)

		sess2 := &store.Session{
			ID:         uuid.New(),
			WorkflowID: wfID,
			Kind:       "ticket",
			Status:     testStatusActive,
		}
		require.NoError(t, s.CreateSession(ctx, sess2))

		got, err := s.GetSessionByWorkflowID(ctx, wfID)
		require.NoError(t, err)
		// should return the most recent one
		assert.Equal(t, sess2.ID, got.ID)
	})

	t.Run("GetByWorkflowID_NotFound", func(t *testing.T) {
		_, err := s.GetSessionByWorkflowID(ctx, "non-existent-workflow-id")
		require.Error(t, err)
		assert.ErrorIs(t, err, pgx.ErrNoRows)
	})

	t.Run("UpdateStatus", func(t *testing.T) {
		sess := &store.Session{
			ID:         uuid.New(),
			WorkflowID: "wf-update-status",
			Kind:       "monitor",
			Status:     testStatusActive,
		}
		require.NoError(t, s.CreateSession(ctx, sess))

		err := s.UpdateSessionStatus(ctx, sess.ID, "completed")
		require.NoError(t, err)

		got, err := s.GetSession(ctx, sess.ID)
		require.NoError(t, err)
		assert.Equal(t, "completed", got.Status)
	})

	t.Run("UpdateStatus_NotFound", func(t *testing.T) {
		err := s.UpdateSessionStatus(ctx, uuid.New(), "completed")
		require.Error(t, err)
		assert.ErrorIs(t, err, pgx.ErrNoRows)
	})

	t.Run("UpdateRunID", func(t *testing.T) {
		sess := &store.Session{
			ID:         uuid.New(),
			WorkflowID: "wf-update-runid",
			Kind:       testKindChat,
			Status:     testStatusActive,
		}
		require.NoError(t, s.CreateSession(ctx, sess))

		err := s.UpdateSessionRunID(ctx, sess.ID, "run-xyz-456")
		require.NoError(t, err)

		got, err := s.GetSession(ctx, sess.ID)
		require.NoError(t, err)
		assert.Equal(t, "run-xyz-456", got.RunID)
	})

	t.Run("ListByStatus", func(t *testing.T) {
		// Use unique workflow IDs to avoid collisions with other test sessions
		prefix := "wf-list-" + uuid.New().String() + "-"

		activeIDs := make(map[uuid.UUID]bool)
		for i := range 3 {
			sess := &store.Session{
				ID:         uuid.New(),
				WorkflowID: prefix + string(rune('a'+i)),
				Kind:       testKindChat,
				Status:     testStatusActive,
			}
			require.NoError(t, s.CreateSession(ctx, sess))
			activeIDs[sess.ID] = true
		}

		completedSess := &store.Session{
			ID:         uuid.New(),
			WorkflowID: prefix + "completed",
			Kind:       testKindChat,
			Status:     "completed",
		}
		require.NoError(t, s.CreateSession(ctx, completedSess))

		list, err := s.ListSessions(ctx, testStatusActive)
		require.NoError(t, err)

		// All returned sessions should have status "active"
		for _, got := range list {
			assert.Equal(t, testStatusActive, got.Status)
		}

		// Our 3 active sessions should be in the list
		foundCount := 0
		for _, got := range list {
			if activeIDs[got.ID] {
				foundCount++
			}
		}
		assert.Equal(t, 3, foundCount)
	})

	t.Run("NullableFields", func(t *testing.T) {
		parentID := uuid.New()
		parent := &store.Session{
			ID:         parentID,
			WorkflowID: "wf-parent",
			Kind:       testKindChat,
			Status:     testStatusActive,
		}
		require.NoError(t, s.CreateSession(ctx, parent))

		child := &store.Session{
			ID:            uuid.New(),
			WorkflowID:    "wf-child",
			Kind:          testKindChat,
			Status:        testStatusActive,
			ParentSession: &parentID,
			EpochSummary:  "summary text",
		}
		require.NoError(t, s.CreateSession(ctx, child))

		got, err := s.GetSession(ctx, child.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ParentSession)
		assert.Equal(t, parentID, *got.ParentSession)
		assert.Equal(t, "summary text", got.EpochSummary)
	})

	t.Run("UpdateSessionEpoch", func(t *testing.T) {
		sess := &store.Session{
			ID:         uuid.New(),
			WorkflowID: "wf-update-epoch",
			Kind:       testKindChat,
			Status:     testStatusActive,
		}
		require.NoError(t, s.CreateSession(ctx, sess))

		err := s.UpdateSessionEpoch(ctx, sess.ID, 2, "epoch 1 summary: resolved ticket #42")
		require.NoError(t, err)

		updated, err := s.GetSession(ctx, sess.ID)
		require.NoError(t, err)
		require.Equal(t, 2, updated.Epoch)
		require.Equal(t, "epoch 1 summary: resolved ticket #42", updated.EpochSummary)
	})

	t.Run("UpdateSessionEpoch_NotFound", func(t *testing.T) {
		err := s.UpdateSessionEpoch(ctx, uuid.New(), 2, "summary")
		require.Error(t, err)
		assert.ErrorIs(t, err, pgx.ErrNoRows)
	})
}

func TestStore_ListChildSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}

	s, _ := newTestStore(t)
	ctx := t.Context()

	t.Run("ReturnsChildren", func(t *testing.T) {
		parentID := uuid.New()
		parent := &store.Session{
			ID:         parentID,
			WorkflowID: "wf-fanout-parent",
			Kind:       store.SessionKindAgentTask,
			Status:     testStatusActive,
		}
		require.NoError(t, s.CreateSession(ctx, parent))

		for i := range 2 {
			child := &store.Session{
				ID:            uuid.New(),
				WorkflowID:    "wf-fanout-child",
				Kind:          store.SessionKindAgentTask,
				Status:        testStatusActive,
				ParentSession: &parentID,
			}
			require.NoError(t, s.CreateSession(ctx, child), "create child %d", i)
		}

		children, err := s.ListChildSessions(ctx, parentID)
		require.NoError(t, err)
		assert.Len(t, children, 2)
		for i := range children {
			require.NotNil(t, children[i].ParentSession)
			assert.Equal(t, parentID, *children[i].ParentSession)
		}
	})

	t.Run("NoChildrenReturnsEmpty", func(t *testing.T) {
		lonelyID := uuid.New()
		lonely := &store.Session{
			ID:         lonelyID,
			WorkflowID: "wf-no-children",
			Kind:       testKindChat,
			Status:     testStatusActive,
		}
		require.NoError(t, s.CreateSession(ctx, lonely))

		children, err := s.ListChildSessions(ctx, lonelyID)
		require.NoError(t, err)
		assert.Empty(t, children)
	})
}

func TestStore_MessageCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}

	s, _ := newTestStore(t)
	ctx := t.Context()

	// Prerequisite: create a session for FK constraint
	sess := &store.Session{
		ID:         uuid.New(),
		WorkflowID: "wf-msg-test-" + uuid.New().String(),
		Kind:       testKindChat,
		Status:     testStatusActive,
	}
	require.NoError(t, s.CreateSession(ctx, sess))

	t.Run("AppendAndGetMessages", func(t *testing.T) {
		meta := json.RawMessage(`{"tool":"recall_memories","args":{"query":"test"}}`)
		msgs := []store.Message{
			{
				ID:            uuid.New(),
				SessionID:     sess.ID,
				Sequence:      1,
				Role:          "user",
				Content:       "Hello, agent!",
				TokenEstimate: 10,
			},
			{
				ID:            uuid.New(),
				SessionID:     sess.ID,
				Sequence:      2,
				Role:          testRoleAssistant,
				Content:       "Hello! How can I help?",
				TokenEstimate: 15,
			},
			{
				ID:            uuid.New(),
				SessionID:     sess.ID,
				Sequence:      3,
				Role:          "tool_call",
				Content:       `{"tool":"recall_memories"}`,
				TokenEstimate: 20,
				Metadata:      meta,
			},
		}

		err := s.AppendMessages(ctx, msgs)
		require.NoError(t, err)

		got, err := s.GetMessages(ctx, sess.ID)
		require.NoError(t, err)
		require.Len(t, got, 3)

		// Verify ordering by sequence ASC
		assert.Equal(t, 1, got[0].Sequence)
		assert.Equal(t, 2, got[1].Sequence)
		assert.Equal(t, 3, got[2].Sequence)

		assert.Equal(t, "user", got[0].Role)
		assert.Equal(t, "Hello, agent!", got[0].Content)
		assert.Equal(t, 10, got[0].TokenEstimate)
		assert.Equal(t, sess.ID, got[0].SessionID)

		assert.Equal(t, "tool_call", got[2].Role)

		// JSONB normalizes key order, so compare semantically not byte-for-byte
		var wantMeta, gotMeta map[string]any
		require.NoError(t, json.Unmarshal(meta, &wantMeta))
		require.NoError(t, json.Unmarshal(got[2].Metadata, &gotMeta))
		assert.Equal(t, wantMeta, gotMeta)
	})

	t.Run("GetTokenCount", func(t *testing.T) {
		total, err := s.GetSessionTokenCount(ctx, sess.ID)
		require.NoError(t, err)
		assert.Equal(t, 45, total) // 10 + 15 + 20
	})

	t.Run("GetMessageCount", func(t *testing.T) {
		count, err := s.GetMessageCount(ctx, sess.ID)
		require.NoError(t, err)
		assert.Equal(t, 3, count)
	})

	t.Run("GetNextSequence", func(t *testing.T) {
		next, err := s.GetNextSequence(ctx, sess.ID)
		require.NoError(t, err)
		assert.Equal(t, 4, next) // MAX(sequence)=3, so 3+1=4
	})

	t.Run("IdempotentAppend", func(t *testing.T) {
		// Re-inserting the same messages should be a no-op (ON CONFLICT DO NOTHING)
		got, err := s.GetMessages(ctx, sess.ID)
		require.NoError(t, err)
		sameMsgs := got // re-use the already-inserted messages

		err = s.AppendMessages(ctx, sameMsgs)
		require.NoError(t, err)

		count, err := s.GetMessageCount(ctx, sess.ID)
		require.NoError(t, err)
		assert.Equal(t, 3, count) // count unchanged
	})

	t.Run("EmptySession", func(t *testing.T) {
		emptySess := &store.Session{
			ID:         uuid.New(),
			WorkflowID: "wf-empty-" + uuid.New().String(),
			Kind:       testKindChat,
			Status:     testStatusActive,
		}
		require.NoError(t, s.CreateSession(ctx, emptySess))

		msgs, err := s.GetMessages(ctx, emptySess.ID)
		require.NoError(t, err)
		assert.Empty(t, msgs)

		total, err := s.GetSessionTokenCount(ctx, emptySess.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, total)

		count, err := s.GetMessageCount(ctx, emptySess.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, count)

		next, err := s.GetNextSequence(ctx, emptySess.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, next) // COALESCE(MAX(sequence), 0) + 1 = 1
	})

	t.Run("AppendEmpty", func(t *testing.T) {
		err := s.AppendMessages(ctx, nil)
		require.NoError(t, err)

		err = s.AppendMessages(ctx, []store.Message{})
		require.NoError(t, err)
	})
}

func TestStore_FullSessionLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}

	s, _ := newTestStore(t)
	ctx := t.Context()

	// 1. Create session (epoch 1)
	sess := store.Session{
		ID:         uuid.New(),
		WorkflowID: "wf-lifecycle-test-" + uuid.New().String(),
		RunID:      "run-1",
		Kind:       testKindChat,
		Status:     testStatusActive,
		Epoch:      1,
	}
	require.NoError(t, s.CreateSession(ctx, &sess), "create session")

	// 2. Append messages simulating a conversation
	msgs := []store.Message{
		{ID: uuid.New(), SessionID: sess.ID, Sequence: 1, Role: "user", Content: "Hello", TokenEstimate: 5},
		{ID: uuid.New(), SessionID: sess.ID, Sequence: 2, Role: testRoleAssistant, Content: "Hi there!", TokenEstimate: 8},
		{ID: uuid.New(), SessionID: sess.ID, Sequence: 3, Role: "tool_call", Content: `{"name":"recall_memories"}`, TokenEstimate: 10},
		{ID: uuid.New(), SessionID: sess.ID, Sequence: 4, Role: "tool_result", Content: "Found 3 memories", TokenEstimate: 15},
		{ID: uuid.New(), SessionID: sess.ID, Sequence: 5, Role: testRoleAssistant, Content: "Based on your memories...", TokenEstimate: 20},
	}
	require.NoError(t, s.AppendMessages(ctx, msgs), "append messages")

	// 3. Verify token count
	tokens, err := s.GetSessionTokenCount(ctx, sess.ID)
	require.NoError(t, err, "get token count")
	assert.Equal(t, 58, tokens, "token count: 5+8+10+15+20=58")

	// 4. Verify message count
	count, err := s.GetMessageCount(ctx, sess.ID)
	require.NoError(t, err, "get message count")
	assert.Equal(t, 5, count, "message count")

	// 5. Simulate epoch transition: create epoch 2 session and mark epoch 1 as continued
	sess2 := store.Session{
		ID:            uuid.New(),
		WorkflowID:    sess.WorkflowID,
		RunID:         "run-2",
		ParentSession: &sess.ID,
		Kind:          testKindChat,
		Status:        testStatusActive,
		Epoch:         2,
		EpochSummary:  "Discussed memories and greetings.",
	}
	require.NoError(t, s.CreateSession(ctx, &sess2), "create epoch 2 session")
	require.NoError(t, s.UpdateSessionStatus(ctx, sess.ID, "continued"), "mark epoch 1 continued")

	tr := store.EpochTransition{
		ID:             uuid.New(),
		FromSession:    sess.ID,
		ToSession:      sess2.ID,
		Trigger:        "token_threshold",
		ExtractedFacts: "User said hello. Found 3 memories.",
		Summary:        "Discussed memories and greetings.",
	}
	require.NoError(t, s.CreateEpochTransition(ctx, &tr), "create epoch transition")

	// 6. Verify epoch chain
	got1, err := s.GetSession(ctx, sess.ID)
	require.NoError(t, err, "get epoch 1 session")
	assert.Equal(t, "continued", got1.Status, "epoch 1 status")

	got2, err := s.GetSession(ctx, sess2.ID)
	require.NoError(t, err, "get epoch 2 session")
	assert.Equal(t, 2, got2.Epoch, "epoch 2 epoch number")
	require.NotNil(t, got2.ParentSession, "epoch 2 parent session should be set")
	assert.Equal(t, sess.ID, *got2.ParentSession, "epoch 2 parent session ID")

	// 7. Verify transitions
	transitions, err := s.GetEpochTransitions(ctx, sess.ID)
	require.NoError(t, err, "get epoch transitions")
	require.Len(t, transitions, 1, "epoch transition count")
	assert.Equal(t, "token_threshold", transitions[0].Trigger, "transition trigger")

	// 8. Complete session
	require.NoError(t, s.UpdateSessionStatus(ctx, sess2.ID, "completed"), "complete session")

	// 9. List active: epoch 1 is continued, epoch 2 is completed; neither is active
	active, err := s.ListSessions(ctx, testStatusActive)
	require.NoError(t, err, "list active sessions")
	for _, a := range active {
		assert.NotEqual(t, sess.ID, a.ID, "epoch 1 should not appear as active")
		assert.NotEqual(t, sess2.ID, a.ID, "epoch 2 should not appear as active")
	}

	// 10. List completed: epoch 2 should appear
	completed, err := s.ListSessions(ctx, "completed")
	require.NoError(t, err, "list completed sessions")
	found := false
	for _, c := range completed {
		if c.ID == sess2.ID {
			found = true
			break
		}
	}
	assert.True(t, found, "epoch 2 session should appear in completed list")
}

func TestMigration003_ErrorRoleAccepted(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}

	s, connStr := newTestStore(t)
	ctx := t.Context()

	// Create a session to own the message.
	sessID := uuid.New()
	err := s.CreateSession(ctx, &store.Session{
		ID:         sessID,
		WorkflowID: "test-wf",
		RunID:      "test-run",
		Kind:       "chat",
		Status:     "active",
		Epoch:      1,
	})
	require.NoError(t, err)

	// Connect directly to verify the CHECK constraint accepts 'error'.
	conn, err := pgx.Connect(ctx, connStr)
	require.NoError(t, err)
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx,
		`INSERT INTO messages (id, session_id, sequence, role, content)
		 VALUES (gen_random_uuid(), $1, 1, 'error', 'boom')`,
		sessID)
	require.NoError(t, err, "error role should be accepted after migration 003")
}

func TestStore_EpochTransition(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}

	s, _ := newTestStore(t)
	ctx := t.Context()

	// Prerequisite: create two sessions (from and to).
	fromSess := &store.Session{
		ID:         uuid.New(),
		WorkflowID: "wf-epoch-from-" + uuid.New().String(),
		Kind:       testKindChat,
		Status:     "continued",
	}
	require.NoError(t, s.CreateSession(ctx, fromSess))

	toSess := &store.Session{
		ID:         uuid.New(),
		WorkflowID: "wf-epoch-to-" + uuid.New().String(),
		Kind:       testKindChat,
		Status:     testStatusActive,
	}
	require.NoError(t, s.CreateSession(ctx, toSess))

	t.Run("CreateAndGet", func(t *testing.T) {
		tr := &store.EpochTransition{
			ID:             uuid.New(),
			FromSession:    fromSess.ID,
			ToSession:      toSess.ID,
			Trigger:        "token_limit",
			ExtractedFacts: "fact1; fact2",
			Summary:        "session summary text",
		}

		err := s.CreateEpochTransition(ctx, tr)
		require.NoError(t, err)
		assert.False(t, tr.CreatedAt.IsZero(), "CreatedAt should be set from RETURNING")

		got, err := s.GetEpochTransitions(ctx, fromSess.ID)
		require.NoError(t, err)
		require.Len(t, got, 1)

		assert.Equal(t, tr.ID, got[0].ID)
		assert.Equal(t, fromSess.ID, got[0].FromSession)
		assert.Equal(t, toSess.ID, got[0].ToSession)
		assert.Equal(t, "token_limit", got[0].Trigger)
		assert.Equal(t, "fact1; fact2", got[0].ExtractedFacts)
		assert.Equal(t, "session summary text", got[0].Summary)
		assert.WithinDuration(t, tr.CreatedAt, got[0].CreatedAt, time.Second)
	})

	t.Run("NullableFields", func(t *testing.T) {
		// Transition with empty extracted_facts and summary (should store as NULL).
		tr := &store.EpochTransition{
			ID:          uuid.New(),
			FromSession: fromSess.ID,
			ToSession:   toSess.ID,
			Trigger:     "manual",
		}

		err := s.CreateEpochTransition(ctx, tr)
		require.NoError(t, err)
		assert.False(t, tr.CreatedAt.IsZero())

		got, err := s.GetEpochTransitions(ctx, fromSess.ID)
		require.NoError(t, err)
		// Two transitions now exist for fromSess
		require.Len(t, got, 2)

		// The second one (ordered by created_at ASC) has empty nullable fields.
		last := got[1]
		assert.Equal(t, "manual", last.Trigger)
		assert.Empty(t, last.ExtractedFacts)
		assert.Empty(t, last.Summary)
	})

	t.Run("EmptyResult", func(t *testing.T) {
		// A session with no transitions should return an empty slice, not an error.
		otherSess := &store.Session{
			ID:         uuid.New(),
			WorkflowID: "wf-no-transitions-" + uuid.New().String(),
			Kind:       testKindChat,
			Status:     testStatusActive,
		}
		require.NoError(t, s.CreateSession(ctx, otherSess))

		got, err := s.GetEpochTransitions(ctx, otherSess.ID)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestMigration004_AgentTaskKindAccepted(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}

	_, connStr := newTestStore(t)
	ctx := t.Context()

	conn, err := pgx.Connect(ctx, connStr)
	require.NoError(t, err)
	defer conn.Close(ctx)

	id := uuid.New()
	_, err = conn.Exec(ctx,
		`INSERT INTO sessions (id, workflow_id, run_id, kind, status, epoch)
		 VALUES ($1, $1::text, $1::text, 'agent_task', 'active', 0)`,
		id)
	require.NoError(t, err, "agent_task kind should be accepted after migration 004")
}
