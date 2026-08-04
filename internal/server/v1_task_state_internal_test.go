package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tphakala/alfred/internal/store"
)

const (
	testTaskName      = "example-task"
	testCandidateKeyA = "record-1"
)

func TestTaskStateItemFromStore(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 8, 12, 0, 0, 0, time.UTC)
	ts := store.TaskState{
		Task:         testTaskName,
		CandidateKey: testCandidateKeyA,
		Status:       store.TaskStatusDone,
		Outcome:      json.RawMessage(`{"result":"handled"}`),
		CaseRunID:    "run-abc",
		Attempts:     2,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	got := taskStateItemFromStore(&ts)

	if got.Task != testTaskName {
		t.Errorf("Task = %q, want %q", got.Task, testTaskName)
	}
	if got.CandidateKey != testCandidateKeyA {
		t.Errorf("CandidateKey = %q, want %q", got.CandidateKey, testCandidateKeyA)
	}
	if got.Status != store.TaskStatusDone {
		t.Errorf("Status = %q, want %q", got.Status, store.TaskStatusDone)
	}
	if got.CaseRunId == nil || *got.CaseRunId != "run-abc" {
		t.Errorf("CaseRunId = %v, want pointer to run-abc", got.CaseRunId)
	}
	if got.Attempts == nil || *got.Attempts != 2 {
		t.Errorf("Attempts = %v, want pointer to 2", got.Attempts)
	}
	if got.CreateTime == nil || !got.CreateTime.Equal(now) {
		t.Errorf("CreateTime = %v, want %v", got.CreateTime, now)
	}
	if got.UpdateTime == nil || !got.UpdateTime.Equal(now) {
		t.Errorf("UpdateTime = %v, want %v", got.UpdateTime, now)
	}
	if got.Outcome == nil {
		t.Fatal("Outcome is nil, want non-nil")
	}
	if string(*got.Outcome) != `{"result":"handled"}` {
		t.Errorf("Outcome = %s, want %s", *got.Outcome, `{"result":"handled"}`)
	}
}

func TestTaskStateItemFromStore_NoOutcomeOrCaseRunID(t *testing.T) {
	t.Parallel()

	ts := store.TaskState{
		Task:         "task-a",
		CandidateKey: "key-1",
		Status:       store.TaskStatusInProgress,
	}

	got := taskStateItemFromStore(&ts)

	if got.CaseRunId != nil {
		t.Errorf("CaseRunId = %v, want nil", got.CaseRunId)
	}
	if got.Outcome != nil {
		t.Errorf("Outcome = %v, want nil", got.Outcome)
	}
}

// TestTaskStateItemFromStore_OutcomeRoundTripsAnyJSON asserts that outcome
// passes through byte-for-byte for any valid JSON value, not just objects.
// outcome is opaque JSON the engine never interprets, so a non-object result
// (an array, a bare string, and so on) must round-trip unchanged rather than
// being silently dropped by a decode-into-object step.
func TestTaskStateItemFromStore_OutcomeRoundTripsAnyJSON(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		outcome json.RawMessage
	}{
		{"array", json.RawMessage(`[1,2,3]`)},
		{"string", json.RawMessage(`"needs a human"`)},
		{"number", json.RawMessage(`42`)},
		{"boolean", json.RawMessage(`false`)},
		{"null", json.RawMessage(`null`)},
		{"jsonObject", json.RawMessage(`{"result":"handled"}`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := store.TaskState{
				Task:         testTaskName,
				CandidateKey: testCandidateKeyA,
				Status:       store.TaskStatusDone,
				Outcome:      tc.outcome,
			}

			got := taskStateItemFromStore(&ts)

			if got.Outcome == nil {
				t.Fatal("Outcome is nil, want non-nil")
			}
			if string(*got.Outcome) != string(tc.outcome) {
				t.Errorf("Outcome = %s, want %s", *got.Outcome, tc.outcome)
			}

			// Also verify it marshals back to identical JSON through the
			// standard encoder, since that is the path the HTTP handler uses.
			marshaled, err := json.Marshal(got.Outcome)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			if string(marshaled) != string(tc.outcome) {
				t.Errorf("json.Marshal(Outcome) = %s, want %s", marshaled, tc.outcome)
			}
		})
	}
}
