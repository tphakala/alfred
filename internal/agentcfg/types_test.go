package agentcfg

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestAgentConfigYAMLRoundTrip(t *testing.T) {
	src := []byte(`
name: memory-seed
strategy: external_agent
runner: claude
description: Manual memory seeding

schedule:
  cron: ""
  randomized_delay: 0s
  persistent: false

concurrency:
  max_concurrent: 1
  on_conflict: skip

budgets:
  workflow_max_cost_usd: 5.00
  workflow_max_wall_clock: 15m

preflight: []

prefilter:
  type: command
  command: "true"

phases:
  - name: main
    model: claude-sonnet-4-6
    runner_flags:
      max_turns: 30
      max_budget_usd: 3.0
    prompt:
      base: prompts/agents/memory-seed.md
    tools:
      bash: ["date*"]
      builtin: [Read, Grep]
      mcp:
        servers:
          hindsight-memory-alfred-dev:
            type: http
            url: http://localhost:8890/mcp/alfred-dev/
    steering:
      mode: scheduled
    output:
      capture: text

state:
  bank: alfred-dev

cleanup: []
`)

	var ac AgentConfig
	require.NoError(t, yaml.Unmarshal(src, &ac))

	assert.Equal(t, "memory-seed", ac.Name)
	assert.Equal(t, "external_agent", ac.Strategy)
	assert.Equal(t, "claude", ac.Runner)
	assert.Equal(t, 1, ac.Concurrency.MaxConcurrent)
	assert.Equal(t, "skip", ac.Concurrency.OnConflict)
	assert.Equal(t, 5.0, ac.Budgets.WorkflowMaxCostUSD)
	assert.Equal(t, 15*time.Minute, ac.Budgets.WorkflowMaxWallClock)
	require.Len(t, ac.Phases, 1)

	p := ac.Phases[0]
	assert.Equal(t, "main", p.Name)
	assert.Equal(t, "claude-sonnet-4-6", p.Model)
	assert.Equal(t, 30, p.RunnerFlags.MaxTurns)
	assert.Equal(t, 3.0, p.RunnerFlags.MaxBudgetUSD)
	assert.Equal(t, []string{"date*"}, p.Tools.Bash)
	assert.Equal(t, []string{"Read", "Grep"}, p.Tools.Builtin)
	require.Contains(t, p.Tools.MCP.Servers, "hindsight-memory-alfred-dev")
	assert.Equal(t, "http", p.Tools.MCP.Servers["hindsight-memory-alfred-dev"].Type)
	assert.Equal(t, "http://localhost:8890/mcp/alfred-dev/", p.Tools.MCP.Servers["hindsight-memory-alfred-dev"].URL)
	assert.Equal(t, "scheduled", string(p.Steering.Mode))
	assert.Equal(t, "alfred-dev", ac.State.Bank)
}

func TestAgentConfigYAMLRoundTrip_Fanout(t *testing.T) {
	src := []byte(`
name: fanout-test
strategy: external_agent
runner: claude

phases:
  - name: scout
    model: claude-haiku-4-5
    prompt: { base: prompts/scout.md }
    steering: { mode: scheduled }
    output: { capture: structured }

  - name: per_issue
    model: claude-opus-4-6
    fanout:
      over: scout.subtasks
      max_parallel: 4
      child:
        model: claude-opus-4-6
        prompt:
          base: prompts/child.md
          variables:
            issue_number: "{{ .item.issue_number }}"
        tools:
          bash: ["git*"]
        runner_flags: { max_turns: 40, max_budget_usd: 2.00 }
        steering: { mode: scheduled }
        output: { capture: text }
    prompt: { base: unused.md }
    steering: { mode: scheduled }
    output: { capture: text }
`)
	var ac AgentConfig
	require.NoError(t, yaml.Unmarshal(src, &ac))
	require.Len(t, ac.Phases, 2)

	p := ac.Phases[1]
	require.NotNil(t, p.Fanout, "fanout should be parsed")
	assert.Equal(t, "scout.subtasks", p.Fanout.Over)
	assert.Equal(t, 4, p.Fanout.MaxParallel)
	assert.Equal(t, "claude-opus-4-6", p.Fanout.Child.Model)
	assert.Equal(t, "prompts/child.md", p.Fanout.Child.Prompt.Base)
	assert.Equal(t, 40, p.Fanout.Child.RunnerFlags.MaxTurns)
	assert.Equal(t, 2.0, p.Fanout.Child.RunnerFlags.MaxBudgetUSD)
}

func TestBackendOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  AgentConfig
		want string
	}{
		{"empty defaults to cli", AgentConfig{}, BackendCLI},
		{"explicit cli", AgentConfig{Backend: BackendCLI}, BackendCLI},
		{"explicit native", AgentConfig{Backend: BackendNative}, BackendNative},
		{"unknown passes through", AgentConfig{Backend: "weird"}, "weird"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := BackendOf(&tt.cfg); got != tt.want {
				t.Fatalf("BackendOf = %q, want %q", got, tt.want)
			}
		})
	}
}
