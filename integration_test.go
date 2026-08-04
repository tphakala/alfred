//go:build integration

package main_test

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/tphakala/alfred/internal/activity"
	"github.com/tphakala/alfred/internal/config"
	"github.com/tphakala/alfred/internal/llm"
	"github.com/tphakala/alfred/internal/memory"
	alfredworkflow "github.com/tphakala/alfred/internal/workflow"
	"go.temporal.io/sdk/testsuite"
)

func TestFullTriageWorkflow(t *testing.T) {
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()

	wfConfig := &config.WorkflowConfig{
		Name:     "ticket-triage",
		Model:    "gemini-3-flash-preview",
		Approval: "none",
		Memory: config.MemoryConfig{
			Bank:         "alfred-tickets",
			RecallBefore: true,
			RetainAfter:  true,
		},
		Steps: []config.WorkflowStep{
			{"analyze": map[string]any{
				"prompt": "Analyze ticket: {{ .Title }}\n{{ .Description }}",
			}},
			{"act": []any{
				map[string]any{"add_note": "Triaged: {{ .category }}/{{ .priority }}"},
			}},
		},
	}

	input := alfredworkflow.WorkflowInput{
		TicketID: 99999,
		TicketData: map[string]any{
			"Title":       "VPN not connecting",
			"Description": "Users in Helsinki office cannot connect to corporate VPN since 09:00",
		},
		WorkflowConfig: wfConfig,
	}

	// Register activity stubs (needed for struct-based activities)
	env.RegisterActivity(&activity.AutotaskActivities{})
	env.RegisterActivity(&activity.LLMActivities{})
	env.RegisterActivity(&activity.MemoryActivities{})

	// Mock recall - returns similar ticket
	env.OnActivity("Recall", mock.Anything, mock.Anything).Return([]memory.MemoryItem{
		{Content: "Similar VPN issue resolved by restarting VPN concentrator", Score: 0.85},
	}, nil)

	// Mock analyze - classifies as network/critical
	env.OnActivity("Analyze", mock.Anything, mock.Anything).Return(&activity.AnalyzeOutput{
		Fields: map[string]any{
			"category":  "network",
			"priority":  "critical",
			"reasoning": "VPN outage affecting entire office",
		},
		Tokens: llm.TokenUsage{TotalTokens: 100},
	}, nil)

	// Mock add note
	env.OnActivity("AddNote", mock.Anything, mock.Anything).Return(nil)

	// Mock retain
	env.OnActivity("Retain", mock.Anything, mock.Anything).Return(nil)

	// Execute
	env.ExecuteWorkflow(alfredworkflow.TicketWorkflow, input)

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}
}
