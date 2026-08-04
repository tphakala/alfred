package agentcfg

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScheduleID_Stable(t *testing.T) {
	assert.Equal(t, "agent_task:memory-seed", ScheduleID("memory-seed"))
}

func TestWorkflowID_StableForFirstRun(t *testing.T) {
	id := WorkflowID("memory-seed", "2026-05-22T10:00:00Z")
	assert.Contains(t, id, "memory-seed")
	assert.Contains(t, id, "2026-05-22T10:00:00Z")
}

func TestCaseWorkflowID_Deterministic(t *testing.T) {
	id1 := CaseWorkflowID("work-queue", "42")
	id2 := CaseWorkflowID("work-queue", "42")
	assert.Equal(t, id1, id2, "same (task, candidateKey) must always derive the same workflow ID")
}

func TestCaseWorkflowID_DistinctPerCandidate(t *testing.T) {
	id1 := CaseWorkflowID("work-queue", "42")
	id2 := CaseWorkflowID("work-queue", "43")
	assert.NotEqual(t, id1, id2)
}

func TestCaseWorkflowID_DistinctPerTask(t *testing.T) {
	id1 := CaseWorkflowID("work-queue-a", "42")
	id2 := CaseWorkflowID("work-queue-b", "42")
	assert.NotEqual(t, id1, id2)
}
