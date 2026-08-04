package agentcfg

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadFile_SupervisedProvingGroundConfig loads the repo's shipped
// proving-ground example of a queue_monitor task with a Case Supervisor
// block (workflows/agents/sentry-triage-supervised.yaml) and asserts it
// stays structurally valid as the schema evolves. The file's own contents
// are a deployment artifact and may name domain specifics; this test only
// asserts schema-level structure, not any domain vocabulary.
func TestLoadFile_SupervisedProvingGroundConfig(t *testing.T) {
	cfg, err := LoadFile("../../workflows/agents/sentry-triage-supervised.yaml")
	require.NoError(t, err, "the shipped proving-ground config must load and validate")

	assert.Equal(t, StrategyQueueMonitor, cfg.Strategy)
	require.NotEmpty(t, cfg.Schedule.Cron, "queue_monitor requires a schedule for the trigger-now path")
	require.NotEmpty(t, cfg.Prefilter.Command)

	require.NotEmpty(t, cfg.Supervisor.Tools, "expected at least one declared command tool")
	for _, tool := range cfg.Supervisor.Tools {
		assert.Contains(t, tool.Command, "--", "tool %q command must contain the flag terminator", tool.Name)
	}

	require.NotEmpty(t, cfg.Supervisor.SubAgents, "expected at least one sub-agent kind")
	for kind, sub := range cfg.Supervisor.SubAgents { //nolint:gocritic // rangeValCopy: mirrors validateSupervisor's pattern, a handful of kinds at most
		assert.NotEmpty(t, sub.Runner, "sub-agent %q must declare a runner", kind)
		assert.NotEmpty(t, sub.Phases, "sub-agent %q must declare at least one phase", kind)
	}

	// Confirmed explicitly, in addition to the assertion embedded in
	// require.NoError above: the shipped config passes Validate.
	require.NoError(t, Validate(&cfg))
}

// TestLoadFile_NativeDemoConfig loads the repo's shipped Phase 4 demo of a
// supervisor mixing both sub-agent backends in one task and asserts the
// schema-level structure: exactly one native kind (with an output schema) and
// one cli kind (with a runner). Domain vocabulary in the file is a deployment
// artifact; this test asserts structure only.
func TestLoadFile_NativeDemoConfig(t *testing.T) {
	cfg, err := LoadFile("../../workflows/agents/native-demo-supervised.yaml")
	require.NoError(t, err, "the shipped demo config must load and validate")
	require.NoError(t, Validate(&cfg))

	require.Len(t, cfg.Supervisor.SubAgents, 2, "the demo mixes exactly two sub-agent kinds")
	var native, cli int
	for kind := range cfg.Supervisor.SubAgents {
		sub := cfg.Supervisor.SubAgents[kind]
		switch BackendOf(&sub) {
		case BackendNative:
			native++
			assert.NotNil(t, sub.Native.Output.Schema, "native kind %q must declare an output schema", kind)
			assert.NotEmpty(t, sub.Native.Prompt.Base, "native kind %q must declare a prompt", kind)
		case BackendCLI:
			cli++
			assert.NotEmpty(t, sub.Runner, "cli kind %q must declare a runner", kind)
			assert.NotEmpty(t, sub.Phases, "cli kind %q must declare at least one phase", kind)
		}
	}
	assert.Equal(t, 1, native, "expected exactly one native kind")
	assert.Equal(t, 1, cli, "expected exactly one cli kind")
}

// TestLoadFile_SandboxIssueTriageConfig loads the sandbox proving-ground
// config: a queue_monitor Case Supervisor task with declared command tools and
// (deliberately, for Milestone 1) no sub-agents. Asserts schema-level structure
// only; the file's domain vocabulary is a deployment artifact.
func TestLoadFile_SandboxIssueTriageConfig(t *testing.T) {
	cfg, err := LoadFile("../../workflows/agents/sandbox-issue-triage.yaml")
	require.NoError(t, err, "the sandbox proving-ground config must load and validate")

	assert.Equal(t, StrategyQueueMonitor, cfg.Strategy)
	require.NotEmpty(t, cfg.Schedule.Cron, "queue_monitor requires a schedule for the trigger-now path")
	require.NotEmpty(t, cfg.Prefilter.Command)

	require.NotEmpty(t, cfg.Supervisor.Tools, "expected the declared command tools")
	for _, tool := range cfg.Supervisor.Tools {
		assert.Contains(t, tool.Command, "--", "tool %q command must contain the flag terminator", tool.Name)
	}
	assert.Empty(t, cfg.Supervisor.SubAgents, "Milestone 1 declares no sub-agents")

	require.NoError(t, Validate(&cfg))
}
