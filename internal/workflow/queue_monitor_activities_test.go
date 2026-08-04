package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/tphakala/alfred/internal/store"
)

// Shared literals reused across this file and case_test.go.
const (
	testCaseTask     = "task-x"
	testCandidateKey = "key-1"
	testRunID1       = "run-1"
	testRunIDABC     = "run-abc"
)

// newTestStore spins up a throwaway Postgres via testcontainers and returns a
// migrated store. It mirrors newTestStore in internal/store/store_test.go.
func newTestStore(t *testing.T) *store.Store {
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
	// t.Context() is already canceled by the time cleanups run, so a
	// Terminate call using it would fail silently and leak the container.
	// context.WithoutCancel keeps the deadline-free parent values without
	// inheriting the cancellation.
	t.Cleanup(func() { _ = pgC.Terminate(context.WithoutCancel(t.Context())) }) //nolint:errcheck // container teardown error is non-actionable in tests

	connStr, err := pgC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	s, err := store.New(ctx, connStr, 5)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(s.Close)

	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return s
}

func TestDedupCandidates_NoRowDispatches(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}
	s := newTestStore(t)
	a := &MonitorActivities{Store: s}
	task := "task-dedup-" + uuid.New().String()

	got, err := a.DedupCandidates(t.Context(), task, []string{"fresh-key"}, 3)
	require.NoError(t, err)
	assert.Equal(t, []string{"fresh-key"}, got.Dispatch)
	assert.Empty(t, got.Skip)
	assert.Empty(t, got.Held)
}

func TestDedupCandidates_TerminalRowsSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}
	s := newTestStore(t)
	a := &MonitorActivities{Store: s}
	task := "task-dedup-terminal-" + uuid.New().String()

	for _, status := range []string{
		store.TaskStatusDone,
		store.TaskStatusSkipped,
		store.TaskStatusEscalated,
		store.TaskStatusNeedsAttention,
	} {
		key := "key-" + status
		_, err := s.UpsertClaim(t.Context(), task, key, testRunID1)
		require.NoError(t, err)
		require.NoError(t, s.FinalizeOutcome(t.Context(), task, key, status, nil))
	}

	got, err := a.DedupCandidates(t.Context(), task, []string{
		"key-" + store.TaskStatusDone,
		"key-" + store.TaskStatusSkipped,
		"key-" + store.TaskStatusEscalated,
		"key-" + store.TaskStatusNeedsAttention,
	}, 3)
	require.NoError(t, err)
	assert.Empty(t, got.Dispatch)
	assert.Empty(t, got.Held)
	assert.Len(t, got.Skip, 4)
}

func TestDedupCandidates_InProgressUnderCapDispatches(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}
	s := newTestStore(t)
	a := &MonitorActivities{Store: s}
	task := "task-dedup-underrap-" + uuid.New().String()
	key := "stuck-key"

	_, err := s.UpsertClaim(t.Context(), task, key, testRunID1)
	require.NoError(t, err)

	got, err := a.DedupCandidates(t.Context(), task, []string{key}, 3)
	require.NoError(t, err)
	assert.Equal(t, []string{key}, got.Dispatch,
		"a prior case that crashed without finalizing must be re-dispatched while under the attempt cap")
	assert.Empty(t, got.Skip)
	assert.Empty(t, got.Held)

	// The row must still be in_progress: DedupCandidates only finalizes rows
	// at or over the attempt cap.
	ts, err := s.GetTaskState(t.Context(), task, key)
	require.NoError(t, err)
	require.NotNil(t, ts)
	assert.Equal(t, store.TaskStatusInProgress, ts.Status)
}

func TestDedupCandidates_InProgressAtCapHeldAndFinalized(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}
	s := newTestStore(t)
	a := &MonitorActivities{Store: s}
	task := "task-dedup-atcap-" + uuid.New().String()
	key := "exhausted-key"

	// Claim three times with distinct run ids so attempts == maxCaseAttempts
	// (3). Each run id models a genuinely new case dispatch; a retry of the
	// same run id would not increment attempts.
	for i := range 3 {
		_, err := s.UpsertClaim(t.Context(), task, key, fmt.Sprintf("run-x-%d", i))
		require.NoError(t, err)
	}

	got, err := a.DedupCandidates(t.Context(), task, []string{key}, 3)
	require.NoError(t, err)
	assert.Empty(t, got.Dispatch)
	assert.Empty(t, got.Skip)
	assert.Equal(t, []string{key}, got.Held)

	ts, err := s.GetTaskState(t.Context(), task, key)
	require.NoError(t, err)
	require.NotNil(t, ts)
	assert.Equal(t, store.TaskStatusNeedsAttention, ts.Status,
		"a candidate at the attempt cap must be finalized needs_attention by the activity itself")
}

func TestDedupCandidates_MixedKeysClassifiedIndependently(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}
	s := newTestStore(t)
	a := &MonitorActivities{Store: s}
	task := "task-dedup-mixed-" + uuid.New().String()

	_, err := s.UpsertClaim(t.Context(), task, "done-key", testRunID1)
	require.NoError(t, err)
	require.NoError(t, s.FinalizeOutcome(t.Context(), task, "done-key", store.TaskStatusDone, nil))

	got, err := a.DedupCandidates(t.Context(), task, []string{"done-key", "new-key"}, 3)
	require.NoError(t, err)
	assert.Equal(t, []string{"new-key"}, got.Dispatch)
	assert.Equal(t, []string{"done-key"}, got.Skip)
	assert.Empty(t, got.Held)
}

func TestCountInProgress_DelegatesToStore(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}
	s := newTestStore(t)
	a := &MonitorActivities{Store: s}
	task := "task-count-" + uuid.New().String()

	_, err := s.UpsertClaim(t.Context(), task, testCandidateKey, testRunID1)
	require.NoError(t, err)

	count, err := a.CountInProgress(t.Context(), task)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestClaimCase_UpsertsLedgerAndCreatesSession(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}
	s := newTestStore(t)
	a := &MonitorActivities{Store: s}
	task := "task-claim-" + uuid.New().String()
	sessionID := uuid.New()

	err := a.ClaimCase(t.Context(), ClaimCaseArgs{
		Task:         task,
		CandidateKey: testCandidateKey,
		SessionID:    sessionID,
		WorkflowID:   "case-" + task + "-key-1",
		RunID:        testRunIDABC,
	})
	require.NoError(t, err)

	ts, err := s.GetTaskState(t.Context(), task, testCandidateKey)
	require.NoError(t, err)
	require.NotNil(t, ts)
	assert.Equal(t, store.TaskStatusInProgress, ts.Status)
	assert.Equal(t, testRunIDABC, ts.CaseRunID)
	assert.Equal(t, 1, ts.Attempts)

	sess, err := s.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, store.SessionKindCase, sess.Kind)
	assert.Equal(t, store.SessionStatusActive, sess.Status)
	assert.Equal(t, testRunIDABC, sess.RunID)
}

func TestClaimCase_RetryDoesNotFailOnExistingSession(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}
	s := newTestStore(t)
	a := &MonitorActivities{Store: s}
	task := "task-claim-retry-" + uuid.New().String()
	sessionID := uuid.New()
	args := ClaimCaseArgs{
		Task:         task,
		CandidateKey: testCandidateKey,
		SessionID:    sessionID,
		WorkflowID:   "case-" + task + "-key-1",
		RunID:        testRunIDABC,
	}

	require.NoError(t, a.ClaimCase(t.Context(), args))
	// A retried activity attempt must not fail on the session row already
	// existing (mirrors CreateAgentSession's IsConflict tolerance).
	require.NoError(t, a.ClaimCase(t.Context(), args))

	ts, err := s.GetTaskState(t.Context(), task, testCandidateKey)
	require.NoError(t, err)
	require.NotNil(t, ts)
	assert.Equal(t, 1, ts.Attempts, "a retry with the same run id must not increment attempts")
}

func TestFinalizeCase_SetsTerminalStatusAndOutcome(t *testing.T) {
	if testing.Short() {
		t.Skip("requires docker")
	}
	s := newTestStore(t)
	a := &MonitorActivities{Store: s}
	task := "task-finalize-" + uuid.New().String()

	_, err := s.UpsertClaim(t.Context(), task, testCandidateKey, testRunID1)
	require.NoError(t, err)

	outcome := json.RawMessage(`{"stub":true}`)
	err = a.FinalizeCase(t.Context(), FinalizeCaseArgs{
		Task:         task,
		CandidateKey: testCandidateKey,
		Status:       store.TaskStatusDone,
		Outcome:      outcome,
	})
	require.NoError(t, err)

	ts, err := s.GetTaskState(t.Context(), task, testCandidateKey)
	require.NoError(t, err)
	require.NotNil(t, ts)
	assert.Equal(t, store.TaskStatusDone, ts.Status)
	assert.JSONEq(t, string(outcome), string(ts.Outcome))
}
