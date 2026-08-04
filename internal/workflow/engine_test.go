package workflow_test

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	"github.com/tphakala/alfred/internal/activity"
	"github.com/tphakala/alfred/internal/config"
	"github.com/tphakala/alfred/internal/memory"
	"github.com/tphakala/alfred/internal/workflow"
)

// Test constants for repeated string literals.
const (
	testModel         = "gemini-flash"
	testApprovalNone  = "none"
	testAnalyzePrompt = "Analyze: {{ .Title }}"
	testStepAnalyze   = "analyze"
	testStepAct       = "act"
	testActionAddNote = "add_note"
	testFieldTitle    = "Title"
	testFieldCategory = "category"
	testFieldPrompt   = "prompt"
)

// TestTicketWorkflow_NoApproval verifies the happy-path workflow when approval
// is not required. It mocks all four activities (Recall, Analyze, AddNote,
// Retain) and asserts the workflow completes without error.
func TestTicketWorkflow_NoApproval(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	wfConfig := &config.WorkflowConfig{
		Name:     "test-triage",
		Model:    testModel,
		Approval: testApprovalNone,
		Memory: config.MemoryConfig{
			Bank:         "test-bank",
			RecallBefore: true,
			RetainAfter:  true,
		},
		Steps: []config.WorkflowStep{
			{testStepAnalyze: map[string]any{testFieldPrompt: testAnalyzePrompt}},
			{testStepAct: []any{
				map[string]any{testActionAddNote: "Result: {{ .category }}"},
			}},
		},
	}

	input := workflow.WorkflowInput{
		TicketID: 12345,
		TicketData: map[string]any{
			testFieldTitle: "Server down",
			"Description":  "Not responding",
		},
		WorkflowConfig: wfConfig,
	}

	// Register the struct activity types so Temporal can resolve method names.
	env.RegisterActivity(&activity.LLMActivities{})
	env.RegisterActivity(&activity.MemoryActivities{})
	env.RegisterActivity(&activity.AutotaskActivities{})

	// Mock Recall — returns an empty memory list.
	env.OnActivity("Recall", mock.Anything, mock.Anything).
		Return([]memory.MemoryItem{}, nil)

	// Mock Analyze — returns a category field.
	env.OnActivity("Analyze", mock.Anything, mock.Anything).
		Return(&activity.AnalyzeOutput{
			Fields: map[string]any{testFieldCategory: "network"},
		}, nil)

	// Mock AddNote — called by the act step.
	env.OnActivity("AddNote", mock.Anything, mock.Anything).
		Return(nil)

	// Mock Retain — called after all steps.
	env.OnActivity("Retain", mock.Anything, mock.Anything).
		Return(nil)

	env.ExecuteWorkflow(workflow.TicketWorkflow, input)

	if !env.IsWorkflowCompleted() {
		t.Fatal("expected workflow to be completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("expected workflow to complete without error, got: %v", err)
	}
}

// TestTicketWorkflow_NoMemory verifies the workflow runs correctly when memory
// integration is disabled (recall_before=false, retain_after=false).
func TestTicketWorkflow_NoMemory(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	wfConfig := &config.WorkflowConfig{
		Name:     "test-no-memory",
		Model:    testModel,
		Approval: testApprovalNone,
		Memory: config.MemoryConfig{
			RecallBefore: false,
			RetainAfter:  false,
		},
		Steps: []config.WorkflowStep{
			{testStepAnalyze: map[string]any{testFieldPrompt: testAnalyzePrompt}},
		},
	}

	input := workflow.WorkflowInput{
		TicketID:       99,
		TicketData:     map[string]any{testFieldTitle: "Printer offline"},
		WorkflowConfig: wfConfig,
	}

	env.RegisterActivity(&activity.LLMActivities{})
	env.RegisterActivity(&activity.MemoryActivities{})
	env.RegisterActivity(&activity.AutotaskActivities{})

	env.OnActivity("Analyze", mock.Anything, mock.Anything).
		Return(&activity.AnalyzeOutput{
			Fields: map[string]any{testFieldCategory: "hardware"},
		}, nil)

	env.ExecuteWorkflow(workflow.TicketWorkflow, input)

	if !env.IsWorkflowCompleted() {
		t.Fatal("expected workflow to be completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("expected workflow to complete without error, got: %v", err)
	}
}

// TestTicketWorkflow_RecallFailureContinues verifies that a Recall activity
// failure is logged and the workflow continues rather than failing.
func TestTicketWorkflow_RecallFailureContinues(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	wfConfig := &config.WorkflowConfig{
		Name:     "test-recall-fail",
		Model:    testModel,
		Approval: testApprovalNone,
		Memory: config.MemoryConfig{
			RecallBefore: true,
			RetainAfter:  false,
		},
		Steps: []config.WorkflowStep{
			{testStepAnalyze: map[string]any{testFieldPrompt: testAnalyzePrompt}},
		},
	}

	input := workflow.WorkflowInput{
		TicketID:       42,
		TicketData:     map[string]any{testFieldTitle: "VPN down", "Description": "Cannot connect"},
		WorkflowConfig: wfConfig,
	}

	env.RegisterActivity(&activity.LLMActivities{})
	env.RegisterActivity(&activity.MemoryActivities{})
	env.RegisterActivity(&activity.AutotaskActivities{})

	// Recall returns an error — workflow should continue.
	env.OnActivity("Recall", mock.Anything, mock.Anything).
		Return([]memory.MemoryItem(nil), errMockRecallFailed)

	env.OnActivity("Analyze", mock.Anything, mock.Anything).
		Return(&activity.AnalyzeOutput{
			Fields: map[string]any{testFieldCategory: "network"},
		}, nil)

	env.ExecuteWorkflow(workflow.TicketWorkflow, input)

	if !env.IsWorkflowCompleted() {
		t.Fatal("expected workflow to be completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("expected workflow to continue despite Recall error, got: %v", err)
	}
}

// TestTicketWorkflow_ApprovalGranted verifies that when approval is required
// and the approval signal is received with Approved=true, the workflow proceeds
// to execute the act step.
func TestTicketWorkflow_ApprovalGranted(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	wfConfig := &config.WorkflowConfig{
		Name:     "test-approval-granted",
		Model:    testModel,
		Approval: "required",
		Memory: config.MemoryConfig{
			RecallBefore: false,
			RetainAfter:  false,
		},
		Steps: []config.WorkflowStep{
			{testStepAct: []any{
				map[string]any{testActionAddNote: "Action executed"},
			}},
		},
	}

	input := workflow.WorkflowInput{
		TicketID:       500,
		TicketData:     map[string]any{testFieldTitle: "Needs approval"},
		WorkflowConfig: wfConfig,
	}

	env.RegisterActivity(&activity.LLMActivities{})
	env.RegisterActivity(&activity.MemoryActivities{})
	env.RegisterActivity(&activity.AutotaskActivities{})

	// AddNote is called twice: once for the approval request, once for the act step.
	env.OnActivity("AddNote", mock.Anything, mock.Anything).Return(nil).Times(2)

	// Send an approved signal after the workflow starts waiting.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflow.ApprovalSignalName, workflow.ApprovalSignal{
			Approved:   true,
			ApproverID: "approver-1",
		})
	}, 0)

	env.ExecuteWorkflow(workflow.TicketWorkflow, input)

	if !env.IsWorkflowCompleted() {
		t.Fatal("expected workflow to be completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("expected workflow to succeed after approval, got: %v", err)
	}
}

// TestTicketWorkflow_ApprovalRejected verifies that when the approval signal
// carries Approved=false, the workflow returns an error.
func TestTicketWorkflow_ApprovalRejected(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	wfConfig := &config.WorkflowConfig{
		Name:     "test-approval-rejected",
		Model:    testModel,
		Approval: "required",
		Memory: config.MemoryConfig{
			RecallBefore: false,
			RetainAfter:  false,
		},
		Steps: []config.WorkflowStep{
			{testStepAct: []any{
				map[string]any{testActionAddNote: "Action executed"},
			}},
		},
	}

	input := workflow.WorkflowInput{
		TicketID:       501,
		TicketData:     map[string]any{testFieldTitle: "Risky change"},
		WorkflowConfig: wfConfig,
	}

	env.RegisterActivity(&activity.LLMActivities{})
	env.RegisterActivity(&activity.MemoryActivities{})
	env.RegisterActivity(&activity.AutotaskActivities{})

	// AddNote called once for the approval request note.
	env.OnActivity("AddNote", mock.Anything, mock.Anything).Return(nil).Once()

	// Send a rejected signal.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflow.ApprovalSignalName, workflow.ApprovalSignal{
			Approved:   false,
			ApproverID: "approver-2",
			Reason:     "too risky",
		})
	}, 0)

	env.ExecuteWorkflow(workflow.TicketWorkflow, input)

	if !env.IsWorkflowCompleted() {
		t.Fatal("expected workflow to be completed")
	}
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("expected workflow to fail on rejection, got nil error")
	}
}

// TestTicketWorkflow_AnalyzeFailure verifies that a failure in the Analyze
// activity propagates and causes the workflow to fail.
func TestTicketWorkflow_AnalyzeFailure(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	wfConfig := &config.WorkflowConfig{
		Name:     "test-analyze-fail",
		Model:    testModel,
		Approval: testApprovalNone,
		Steps: []config.WorkflowStep{
			{testStepAnalyze: map[string]any{testFieldPrompt: testAnalyzePrompt}},
		},
	}

	input := workflow.WorkflowInput{
		TicketID:       77,
		TicketData:     map[string]any{testFieldTitle: "LLM will fail"},
		WorkflowConfig: wfConfig,
	}

	env.RegisterActivity(&activity.LLMActivities{})
	env.RegisterActivity(&activity.MemoryActivities{})
	env.RegisterActivity(&activity.AutotaskActivities{})

	env.OnActivity("Analyze", mock.Anything, mock.Anything).
		Return((*activity.AnalyzeOutput)(nil), &mockError{"LLM unavailable"})

	env.ExecuteWorkflow(workflow.TicketWorkflow, input)

	if !env.IsWorkflowCompleted() {
		t.Fatal("expected workflow to be completed")
	}
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("expected workflow to fail when Analyze fails, got nil error")
	}
}

// TestTicketWorkflow_RetainFailureContinues verifies that a Retain activity
// failure is logged and the workflow succeeds rather than failing.
func TestTicketWorkflow_RetainFailureContinues(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	wfConfig := &config.WorkflowConfig{
		Name:     "test-retain-fail",
		Model:    testModel,
		Approval: testApprovalNone,
		Memory: config.MemoryConfig{
			RecallBefore: false,
			RetainAfter:  true,
		},
		Steps: []config.WorkflowStep{
			{testStepAnalyze: map[string]any{testFieldPrompt: testAnalyzePrompt}},
		},
	}

	input := workflow.WorkflowInput{
		TicketID:       88,
		TicketData:     map[string]any{testFieldTitle: "Retain will fail"},
		WorkflowConfig: wfConfig,
	}

	env.RegisterActivity(&activity.LLMActivities{})
	env.RegisterActivity(&activity.MemoryActivities{})
	env.RegisterActivity(&activity.AutotaskActivities{})

	env.OnActivity("Analyze", mock.Anything, mock.Anything).
		Return(&activity.AnalyzeOutput{Fields: map[string]any{testFieldCategory: "test"}}, nil)

	env.OnActivity("Retain", mock.Anything, mock.Anything).
		Return(&mockError{"retain service down"})

	env.ExecuteWorkflow(workflow.TicketWorkflow, input)

	if !env.IsWorkflowCompleted() {
		t.Fatal("expected workflow to be completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("expected workflow to succeed despite Retain failure, got: %v", err)
	}
}

// errMockRecallFailed is a sentinel error for the recall-failure test.
var errMockRecallFailed = &mockError{"recall service unavailable"}

type mockError struct{ msg string }

func (e *mockError) Error() string { return e.msg }
