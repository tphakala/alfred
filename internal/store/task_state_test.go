package store_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tphakala/alfred/internal/store"
)

const testCandidateKey1 = "candidate-1"

func TestStore_TaskState(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}

	s, _ := newTestStore(t)
	ctx := t.Context()

	t.Run("UpsertClaim_InsertsThenIncrementsAttempts", func(t *testing.T) {
		task := "task-claim-" + uuid.New().String()
		key := testCandidateKey1

		attempts, err := s.UpsertClaim(ctx, task, key, "run-1")
		require.NoError(t, err)
		assert.Equal(t, 1, attempts)

		got, err := s.GetTaskState(ctx, task, key)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, store.TaskStatusInProgress, got.Status)
		assert.Equal(t, "run-1", got.CaseRunID)
		assert.Equal(t, 1, got.Attempts)

		attempts, err = s.UpsertClaim(ctx, task, key, "run-2")
		require.NoError(t, err)
		assert.Equal(t, 2, attempts)

		got, err = s.GetTaskState(ctx, task, key)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, store.TaskStatusInProgress, got.Status)
		assert.Equal(t, "run-2", got.CaseRunID)
		assert.Equal(t, 2, got.Attempts)
	})

	t.Run("UpsertClaim_SameRunIDDoesNotIncrementAttempts", func(t *testing.T) {
		task := "task-claim-retry-" + uuid.New().String()
		key := testCandidateKey1

		attempts, err := s.UpsertClaim(ctx, task, key, "run-1")
		require.NoError(t, err)
		assert.Equal(t, 1, attempts)

		// A retry of the same activity attempt (same run id) must not
		// inflate attempts.
		attempts, err = s.UpsertClaim(ctx, task, key, "run-1")
		require.NoError(t, err)
		assert.Equal(t, 1, attempts)

		// A genuinely new dispatch (different run id) does increment.
		attempts, err = s.UpsertClaim(ctx, task, key, "run-2")
		require.NoError(t, err)
		assert.Equal(t, 2, attempts)
	})

	t.Run("FinalizeOutcome_SetsTerminalStatusAndOutcome", func(t *testing.T) {
		task := "task-finalize-" + uuid.New().String()
		key := testCandidateKey1

		_, err := s.UpsertClaim(ctx, task, key, "run-1")
		require.NoError(t, err)

		outcome := json.RawMessage(`{"result":"handled"}`)
		err = s.FinalizeOutcome(ctx, task, key, store.TaskStatusDone, outcome)
		require.NoError(t, err)

		got, err := s.GetTaskState(ctx, task, key)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, store.TaskStatusDone, got.Status)

		var wantOutcome, gotOutcome map[string]any
		require.NoError(t, json.Unmarshal(outcome, &wantOutcome))
		require.NoError(t, json.Unmarshal(got.Outcome, &gotOutcome))
		assert.Equal(t, wantOutcome, gotOutcome)
	})

	t.Run("FinalizeOutcome_MissingKeyErrors", func(t *testing.T) {
		task := "task-finalize-missing-" + uuid.New().String()

		err := s.FinalizeOutcome(ctx, task, "no-such-key", store.TaskStatusDone, nil)
		require.Error(t, err)
	})

	t.Run("GetTaskState_AbsentKeyReturnsNilNil", func(t *testing.T) {
		task := "task-absent-" + uuid.New().String()

		got, err := s.GetTaskState(ctx, task, "no-such-key")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("ListTaskState_FiltersByStatusAndPagesByUpdatedAtDesc", func(t *testing.T) {
		task := "task-list-" + uuid.New().String()

		// key-1: in_progress (claimed once).
		_, err := s.UpsertClaim(ctx, task, "key-1", "run-1")
		require.NoError(t, err)

		// key-2: claimed then finalized as done.
		_, err = s.UpsertClaim(ctx, task, "key-2", "run-2")
		require.NoError(t, err)
		require.NoError(t, s.FinalizeOutcome(ctx, task, "key-2", store.TaskStatusDone, nil))

		// key-3: claimed then finalized as skipped.
		_, err = s.UpsertClaim(ctx, task, "key-3", "run-3")
		require.NoError(t, err)
		require.NoError(t, s.FinalizeOutcome(ctx, task, "key-3", store.TaskStatusSkipped, nil))

		all, err := s.ListTaskState(ctx, task, "", 10, 0)
		require.NoError(t, err)
		require.Len(t, all, 3)
		// updated_at DESC: the most recently touched row (key-3) comes first.
		assert.Equal(t, "key-3", all[0].CandidateKey)

		done, err := s.ListTaskState(ctx, task, store.TaskStatusDone, 10, 0)
		require.NoError(t, err)
		require.Len(t, done, 1)
		assert.Equal(t, "key-2", done[0].CandidateKey)

		page1, err := s.ListTaskState(ctx, task, "", 2, 0)
		require.NoError(t, err)
		assert.Len(t, page1, 2)

		page2, err := s.ListTaskState(ctx, task, "", 2, 2)
		require.NoError(t, err)
		assert.Len(t, page2, 1)
	})

	t.Run("CountInProgress_ScopesToTaskAndStatus", func(t *testing.T) {
		taskA := "task-count-a-" + uuid.New().String()
		taskB := "task-count-b-" + uuid.New().String()

		_, err := s.UpsertClaim(ctx, taskA, "key-1", "run-1")
		require.NoError(t, err)
		_, err = s.UpsertClaim(ctx, taskA, "key-2", "run-2")
		require.NoError(t, err)
		require.NoError(t, s.FinalizeOutcome(ctx, taskA, "key-2", store.TaskStatusDone, nil))

		_, err = s.UpsertClaim(ctx, taskB, "key-1", "run-3")
		require.NoError(t, err)

		count, err := s.CountInProgress(ctx, taskA)
		require.NoError(t, err)
		assert.Equal(t, 1, count)

		countB, err := s.CountInProgress(ctx, taskB)
		require.NoError(t, err)
		assert.Equal(t, 1, countB)
	})

	t.Run("RequeueTaskState_DeletesRowThenReturnsFalse", func(t *testing.T) {
		task := "task-requeue-" + uuid.New().String()
		key := testCandidateKey1

		_, err := s.UpsertClaim(ctx, task, key, "run-1")
		require.NoError(t, err)
		require.NoError(t, s.FinalizeOutcome(ctx, task, key, store.TaskStatusNeedsAttention, nil))

		deleted, err := s.RequeueTaskState(ctx, task, key)
		require.NoError(t, err)
		assert.True(t, deleted)

		got, err := s.GetTaskState(ctx, task, key)
		require.NoError(t, err)
		assert.Nil(t, got)

		deleted, err = s.RequeueTaskState(ctx, task, key)
		require.NoError(t, err)
		assert.False(t, deleted)
	})
}

// TestIsTerminalTaskStatus does not require docker: it exercises the
// terminal-status set directly, no store instance needed.
func TestIsTerminalTaskStatus(t *testing.T) {
	for _, status := range []string{
		store.TaskStatusDone,
		store.TaskStatusSkipped,
		store.TaskStatusEscalated,
		store.TaskStatusNeedsAttention,
	} {
		assert.True(t, store.IsTerminalTaskStatus(status), "status %q must be terminal", status)
	}
	assert.False(t, store.IsTerminalTaskStatus(store.TaskStatusInProgress))
	assert.False(t, store.IsTerminalTaskStatus("bogus"))

	terminal := store.TerminalTaskStatuses()
	assert.ElementsMatch(t, []string{
		store.TaskStatusDone,
		store.TaskStatusSkipped,
		store.TaskStatusEscalated,
		store.TaskStatusNeedsAttention,
	}, terminal)

	// The returned slice is a copy: mutating it must not affect the next call.
	terminal[0] = "mutated"
	assert.NotEqual(t, terminal, store.TerminalTaskStatuses())
}
