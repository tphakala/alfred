package workflow_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/llm"
	wf "github.com/tphakala/alfred/internal/workflow"
)

const (
	testCaseUserMsg        = "handle this case"
	testEchoBin            = "/bin/echo"
	testEchoToolName       = "echo_tool"
	testArgAction          = "action"
	testEnvTokenKey        = "TOKEN"
	testPlaceholderMessage = "{message}"
)

// --- CaseLLMStream ---

func TestCaseActivities_CaseLLMStream_NilLLMClient(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.CaseActivities{}
	env.RegisterActivity(activities)

	_, err := env.ExecuteActivity(activities.CaseLLMStream, wf.CaseLLMStreamRequest{SessionID: uuid.New()})
	require.Error(t, err)
}

func TestCaseActivities_CaseLLMStream_AccumulatesTextAndToolCalls(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	mockLLM := &llm.MockClient{
		MockStream: []llm.StreamChunk{
			{Text: "Investigating "},
			{
				Text: "the case",
				ToolCalls: []llm.ToolCallPart{
					{Name: agentcfg.ReservedToolSpawnAgent, Args: map[string]any{"kind": "investigate"}},
				},
			},
			{Tokens: &llm.TokenUsage{PromptTokens: 10, ResponseTokens: 20, TotalTokens: 30}},
		},
	}

	activities := &wf.CaseActivities{LLM: mockLLM, DefaultModel: "default-model"}
	env.RegisterActivity(activities)

	req := wf.CaseLLMStreamRequest{
		SessionID: uuid.New(),
		TurnID:    1,
		Round:     2,
		Messages:  []wf.ContextMessage{{Role: testRoleUser, Content: testCaseUserMsg}},
	}

	val, err := env.ExecuteActivity(activities.CaseLLMStream, req)
	require.NoError(t, err)

	var result wf.CaseLLMStreamResult
	require.NoError(t, val.Get(&result))

	assert.Equal(t, "Investigating the case", result.Text)
	require.Len(t, result.ToolCalls, 1)
	assert.Equal(t, "t2-c0", result.ToolCalls[0].CallID, "CallID must follow the t{round}-c{index} scheme")
	assert.Equal(t, agentcfg.ReservedToolSpawnAgent, result.ToolCalls[0].Name)
	require.NotNil(t, result.Usage)
	assert.Equal(t, int32(30), result.Usage.TotalTokens)
}

func TestCaseActivities_CaseLLMStream_NoBakedInSystemPrompt(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	mockLLM := &llm.MockClient{}
	activities := &wf.CaseActivities{LLM: mockLLM, DefaultModel: "default-model"}
	env.RegisterActivity(activities)

	req := wf.CaseLLMStreamRequest{
		SessionID:         uuid.New(),
		Round:             1,
		Messages:          []wf.ContextMessage{{Role: testRoleUser, Content: testCaseUserMsg}},
		SystemInstruction: "You are the case supervisor for task X.",
	}

	_, err := env.ExecuteActivity(activities.CaseLLMStream, req)
	require.NoError(t, err)

	require.NotNil(t, mockLLM.LastStreamRequest)
	assert.Equal(t, "You are the case supervisor for task X.", mockLLM.LastStreamRequest.SystemInstruction,
		"CaseLLMStream must not prepend any baked-in system prompt, unlike chat's LLMStream")
}

func TestCaseActivities_CaseLLMStream_ModelFallsBackToDefault(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	mockLLM := &llm.MockClient{}
	activities := &wf.CaseActivities{LLM: mockLLM, DefaultModel: "fallback-model"}
	env.RegisterActivity(activities)

	req := wf.CaseLLMStreamRequest{
		SessionID: uuid.New(),
		Round:     1,
		Messages:  []wf.ContextMessage{{Role: testRoleUser, Content: testCaseUserMsg}},
	}

	_, err := env.ExecuteActivity(activities.CaseLLMStream, req)
	require.NoError(t, err)
	require.NotNil(t, mockLLM.LastStreamRequest)
	assert.Equal(t, "fallback-model", mockLLM.LastStreamRequest.Model)
}

func TestCaseActivities_CaseLLMStream_UsesRequestModelOverDefault(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	mockLLM := &llm.MockClient{}
	activities := &wf.CaseActivities{LLM: mockLLM, DefaultModel: "fallback-model"}
	env.RegisterActivity(activities)

	req := wf.CaseLLMStreamRequest{
		SessionID: uuid.New(),
		Round:     1,
		Messages:  []wf.ContextMessage{{Role: testRoleUser, Content: testCaseUserMsg}},
		Model:     "supervisor-configured-model",
	}

	_, err := env.ExecuteActivity(activities.CaseLLMStream, req)
	require.NoError(t, err)
	require.NotNil(t, mockLLM.LastStreamRequest)
	assert.Equal(t, "supervisor-configured-model", mockLLM.LastStreamRequest.Model)
}

func TestCaseActivities_CaseLLMStream_MetersCostFromPricing(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	mockLLM := &llm.MockClient{
		MockStream: []llm.StreamChunk{
			{Tokens: &llm.TokenUsage{PromptTokens: 1_000_000, ResponseTokens: 1_000_000, TotalTokens: 2_000_000}},
		},
	}
	activities := &wf.CaseActivities{
		LLM:          mockLLM,
		DefaultModel: "gemini-2.0-flash",
		Pricing:      llm.PriceTable{"gemini-2.0-flash": {InputPerMillion: 0.10, OutputPerMillion: 0.40}},
	}
	env.RegisterActivity(activities)

	req := wf.CaseLLMStreamRequest{
		SessionID: uuid.New(),
		Round:     1,
		Messages:  []wf.ContextMessage{{Role: testRoleUser, Content: testCaseUserMsg}},
	}
	val, err := env.ExecuteActivity(activities.CaseLLMStream, req)
	require.NoError(t, err)

	var result wf.CaseLLMStreamResult
	require.NoError(t, val.Get(&result))
	// 1M input @ 0.10/M + 1M output @ 0.40/M = 0.50.
	assert.InDelta(t, 0.50, result.CostUSD, 1e-9)
}

func TestCaseActivities_CaseLLMStream_UnpricedModelIsZeroCost(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	mockLLM := &llm.MockClient{
		MockStream: []llm.StreamChunk{
			{Tokens: &llm.TokenUsage{PromptTokens: 1_000_000, ResponseTokens: 1_000_000}},
		},
	}
	activities := &wf.CaseActivities{
		LLM:          mockLLM,
		DefaultModel: "some-unlisted-model",
		Pricing:      llm.PriceTable{"gemini-2.0-flash": {InputPerMillion: 0.10, OutputPerMillion: 0.40}},
	}
	env.RegisterActivity(activities)

	req := wf.CaseLLMStreamRequest{
		SessionID: uuid.New(),
		Round:     1,
		Messages:  []wf.ContextMessage{{Role: testRoleUser, Content: testCaseUserMsg}},
	}
	val, err := env.ExecuteActivity(activities.CaseLLMStream, req)
	require.NoError(t, err)

	var result wf.CaseLLMStreamResult
	require.NoError(t, val.Get(&result))
	assert.Zero(t, result.CostUSD, "an unpriced model must contribute zero to the cost cap")
}

func TestCaseActivities_CaseLLMStream_NilUsageIsZeroCost(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	// Provider reports no token counts (no Tokens chunk), so usage is nil and
	// there is nothing to price even though a table is configured.
	mockLLM := &llm.MockClient{MockStream: []llm.StreamChunk{{Text: "hi"}}}
	activities := &wf.CaseActivities{
		LLM:          mockLLM,
		DefaultModel: "gemini-2.0-flash",
		Pricing:      llm.PriceTable{"gemini-2.0-flash": {InputPerMillion: 0.10, OutputPerMillion: 0.40}},
	}
	env.RegisterActivity(activities)

	val, err := env.ExecuteActivity(activities.CaseLLMStream, wf.CaseLLMStreamRequest{
		SessionID: uuid.New(), Round: 1,
		Messages: []wf.ContextMessage{{Role: testRoleUser, Content: testCaseUserMsg}},
	})
	require.NoError(t, err)
	var result wf.CaseLLMStreamResult
	require.NoError(t, val.Get(&result))
	assert.Zero(t, result.CostUSD, "no token usage means no metered cost")
}

func TestCaseActivities_CaseLLMStream_NilPricingIsZeroCost(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	// Metering disabled (nil Pricing): usage is present but nothing prices it.
	mockLLM := &llm.MockClient{MockStream: []llm.StreamChunk{
		{Tokens: &llm.TokenUsage{PromptTokens: 1_000_000, ResponseTokens: 1_000_000}},
	}}
	activities := &wf.CaseActivities{LLM: mockLLM, DefaultModel: "gemini-2.0-flash"}
	env.RegisterActivity(activities)

	val, err := env.ExecuteActivity(activities.CaseLLMStream, wf.CaseLLMStreamRequest{
		SessionID: uuid.New(), Round: 1,
		Messages: []wf.ContextMessage{{Role: testRoleUser, Content: testCaseUserMsg}},
	})
	require.NoError(t, err)
	var result wf.CaseLLMStreamResult
	require.NoError(t, val.Get(&result))
	assert.Zero(t, result.CostUSD, "nil pricing table means metering is disabled")
}

// --- RunDeclaredCommand ---

func TestCaseActivities_RunDeclaredCommand_SubstitutesArgAsDiscreteElement(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.CaseActivities{}
	env.RegisterActivity(activities)

	in := wf.RunDeclaredCommandArgs{
		Tool: agentcfg.DeclaredTool{
			Name:    testEchoToolName,
			Command: []string{testEchoBin, "--", testPlaceholderMessage},
		},
		Args: map[string]any{"message": "hello there, has spaces"},
	}

	val, err := env.ExecuteActivity(activities.RunDeclaredCommand, in)
	require.NoError(t, err)

	var result wf.RunDeclaredCommandResult
	require.NoError(t, val.Get(&result))
	assert.Equal(t, 0, result.ExitCode)
	// The "--" element is passed through to the child like any other literal
	// argv element (it is our injection-guard boundary, not something the
	// engine strips before exec'ing); /bin/echo just prints all its args.
	assert.Equal(t, "-- hello there, has spaces\n", result.Stdout,
		"a value containing spaces must arrive as one argv element, not be word-split")
}

func TestCaseActivities_RunDeclaredCommand_PreDashDashInjectionRejected(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.CaseActivities{}
	env.RegisterActivity(activities)

	in := wf.RunDeclaredCommandArgs{
		Tool: agentcfg.DeclaredTool{
			Name:    "lookup_tool",
			Command: []string{testEchoBin, "{action}", "--", "trailer"},
		},
		Args: map[string]any{testArgAction: "--delete-all"},
	}

	val, err := env.ExecuteActivity(activities.RunDeclaredCommand, in)
	require.NoError(t, err, "a rejected call is a normal result, not an activity error")

	var result wf.RunDeclaredCommandResult
	require.NoError(t, val.Get(&result))
	assert.NotEqual(t, 0, result.ExitCode, "the command must not have been treated as a clean success")
	assert.Contains(t, result.Stderr, testArgAction)
	assert.Empty(t, result.Stdout, "the command must never have executed")
}

func TestCaseActivities_RunDeclaredCommand_PostDashDashDashPrefixAllowed(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.CaseActivities{}
	env.RegisterActivity(activities)

	in := wf.RunDeclaredCommandArgs{
		Tool: agentcfg.DeclaredTool{
			Name:    testEchoToolName,
			Command: []string{testEchoBin, "--", "{value}"},
		},
		Args: map[string]any{"value": "-not-a-flag"},
	}

	val, err := env.ExecuteActivity(activities.RunDeclaredCommand, in)
	require.NoError(t, err)

	var result wf.RunDeclaredCommandResult
	require.NoError(t, val.Get(&result))
	assert.Equal(t, 0, result.ExitCode, "a value starting with - is fine after the -- terminator")
	assert.Equal(t, "-- -not-a-flag\n", result.Stdout)
}

func TestCaseActivities_RunDeclaredCommand_MissingPlaceholderValueRejected(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.CaseActivities{}
	env.RegisterActivity(activities)

	in := wf.RunDeclaredCommandArgs{
		Tool: agentcfg.DeclaredTool{
			Name:    testEchoToolName,
			Command: []string{testEchoBin, "--", testPlaceholderMessage},
		},
		Args: map[string]any{},
	}

	val, err := env.ExecuteActivity(activities.RunDeclaredCommand, in)
	require.NoError(t, err, "a missing argument is a normal result, not an activity error")

	var result wf.RunDeclaredCommandResult
	require.NoError(t, val.Get(&result))
	assert.NotEqual(t, 0, result.ExitCode)
	assert.Contains(t, result.Stderr, "message")
}

func TestCaseActivities_RunDeclaredCommand_MissingTerminatorReturnsActivityError(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.CaseActivities{}
	env.RegisterActivity(activities)

	in := wf.RunDeclaredCommandArgs{
		Tool: agentcfg.DeclaredTool{
			Name:    "misconfigured_tool",
			Command: []string{testEchoBin, testPlaceholderMessage}, // no "--"
		},
		Args: map[string]any{"message": "hi"},
	}

	_, err := env.ExecuteActivity(activities.RunDeclaredCommand, in)
	require.Error(t, err, "a command missing the mandatory -- terminator is a configuration defect, not an LLM-arg problem")
}

func TestCaseActivities_RunDeclaredCommand_SecretEnvHydrated(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.CaseActivities{Secrets: map[string]string{"lookup-token": "s3cr3t-value"}}
	env.RegisterActivity(activities)

	in := wf.RunDeclaredCommandArgs{
		Tool: agentcfg.DeclaredTool{
			Name:    "env_tool",
			Command: []string{"/usr/bin/printenv", "--", testEnvTokenKey},
			Env:     map[string]string{testEnvTokenKey: "${secret:lookup-token}"},
		},
		Args: map[string]any{},
	}

	val, err := env.ExecuteActivity(activities.RunDeclaredCommand, in)
	require.NoError(t, err)

	var result wf.RunDeclaredCommandResult
	require.NoError(t, val.Get(&result))
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "s3cr3t-value\n", result.Stdout)
}

func TestCaseActivities_RunDeclaredCommand_MissingSecretReturnsActivityError(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.CaseActivities{Secrets: map[string]string{}} // secret not present
	env.RegisterActivity(activities)

	in := wf.RunDeclaredCommandArgs{
		Tool: agentcfg.DeclaredTool{
			Name:    "env_tool",
			Command: []string{"/usr/bin/printenv", "--", testEnvTokenKey},
			Env:     map[string]string{testEnvTokenKey: "${secret:missing-token}"},
		},
		Args: map[string]any{},
	}

	_, err := env.ExecuteActivity(activities.RunDeclaredCommand, in)
	require.Error(t, err, "a command must not run without a declared credential (fail closed)")
}

func TestCaseActivities_RunDeclaredCommand_NonZeroExitCapturedNotAnError(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.CaseActivities{}
	env.RegisterActivity(activities)

	in := wf.RunDeclaredCommandArgs{
		Tool: agentcfg.DeclaredTool{
			Name:    "false_tool",
			Command: []string{"/bin/false", "--"},
		},
		Args: map[string]any{},
	}

	val, err := env.ExecuteActivity(activities.RunDeclaredCommand, in)
	require.NoError(t, err, "a non-zero process exit is not an activity error; the LLM sees it in the result")

	var result wf.RunDeclaredCommandResult
	require.NoError(t, val.Get(&result))
	assert.Equal(t, 1, result.ExitCode)
}

// --- RenderCasePrompt ---

func TestCaseActivities_RenderCasePrompt_RendersBaseFile(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.CaseActivities{}
	env.RegisterActivity(activities)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.md"), []byte("supervise the task"), 0o644))

	val, err := env.ExecuteActivity(activities.RenderCasePrompt, dir, agentcfg.Prompt{Base: "base.md"}, map[string]any(nil))
	require.NoError(t, err)

	var out string
	require.NoError(t, val.Get(&out))
	assert.Equal(t, "supervise the task", out)
}

func TestCaseActivities_RenderCasePrompt_MissingBaseFileReturnsError(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()

	activities := &wf.CaseActivities{}
	env.RegisterActivity(activities)

	dir := t.TempDir()

	_, err := env.ExecuteActivity(activities.RenderCasePrompt, dir, agentcfg.Prompt{Base: "missing.md"}, map[string]any(nil))
	require.Error(t, err)
}
