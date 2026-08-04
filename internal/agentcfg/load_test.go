package agentcfg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadFromDir_Valid(t *testing.T) {
	configs, err := LoadFromDir("testdata")
	// err may be non-nil because invalid-no-name.yaml fails validation,
	// but valid configs should still be returned.
	require.Len(t, configs, 1)
	assert.Equal(t, "test-config", configs[0].Name)
	_ = err
}

func TestLoadFromDir_RealConfigs(t *testing.T) {
	configs, err := LoadFromDir("../../workflows/agents")
	require.NoError(t, err, "all real agent configs should validate")
	require.GreaterOrEqual(t, len(configs), 3, "expected at least 3 configs (memory-seed, fanout-demo, approval-demo)")

	names := make(map[string]bool, len(configs))
	for _, c := range configs {
		names[c.Name] = true
	}
	assert.True(t, names["fanout-demo"], "fanout-demo config must load")
	assert.True(t, names["approval-demo"], "approval-demo config must load")
	assert.True(t, names["memory-seed"], "memory-seed config must load")

	// Verify fanout-demo has the expected structure.
	for _, c := range configs {
		if c.Name == "fanout-demo" {
			require.Len(t, c.Phases, 2)
			assert.Equal(t, "scout", c.Phases[0].Name)
			assert.Equal(t, "per_item", c.Phases[1].Name)
			require.NotNil(t, c.Phases[1].Fanout)
			assert.Equal(t, "scout.items", c.Phases[1].Fanout.Over)
			assert.Equal(t, 2, c.Phases[1].Fanout.MaxParallel)
		}
		if c.Name == "approval-demo" {
			require.Len(t, c.Phases, 1)
			assert.Equal(t, SteeringInteractive, c.Phases[0].Steering.Mode)
		}
	}
}

// writeAgentYAML writes a minimal valid external_agent config declaring the
// given top-level name into dir under fileName.
func writeAgentYAML(t *testing.T, dir, fileName, cfgName string) {
	t.Helper()
	content := `name: ` + cfgName + `
strategy: external_agent
runner: claude
phases:
  - name: main
    model: test-model
    prompt:
      base: prompts/test.md
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0o600))
}

func TestLoadFromDir_RejectsDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	writeAgentYAML(t, dir, "a.yaml", "dup-task")
	writeAgentYAML(t, dir, "b.yaml", "dup-task")
	writeAgentYAML(t, dir, "c.yaml", "unique-task")

	configs, err := LoadFromDir(dir)

	// Every by-name lookup over the loaded slice is first-match-wins, so a
	// duplicated name could resolve to the wrong file's config (wrong MCP
	// secrets injected into another task's runner). Fail closed: neither
	// same-named config may load; unrelated configs still do.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dup-task")
	assert.Contains(t, err.Error(), "a.yaml")
	assert.Contains(t, err.Error(), "b.yaml")

	require.Len(t, configs, 1)
	assert.Equal(t, "unique-task", configs[0].Name)
}

func TestLoadFromDir_DuplicateNameWithFailedFileLoadsSurvivor(t *testing.T) {
	dir := t.TempDir()
	writeAgentYAML(t, dir, "a.yaml", "shared-name")
	// Same name, but the file fails validation (no runner), so it never
	// enters the returned slice and takes no part in duplicate detection.
	broken := "name: shared-name\nstrategy: external_agent\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(broken), 0o600))

	configs, err := LoadFromDir(dir)

	// Pin the intended semantics: duplicate detection covers successfully
	// loaded configs only. The broken file is reported as a per-file error,
	// and the valid config still loads because the returned slice holds no
	// ambiguous name (by-name lookups cannot resolve to a config that never
	// loaded).
	require.Error(t, err)
	assert.Contains(t, err.Error(), "b.yaml")
	assert.NotContains(t, err.Error(), "duplicate config name")
	require.Len(t, configs, 1)
	assert.Equal(t, "shared-name", configs[0].Name)
}

func TestValidate_FanoutOverOwnPhaseRejected(t *testing.T) {
	cfg := AgentConfig{
		Name:     "x",
		Strategy: "external_agent",
		Runner:   "claude",
		Phases: []Phase{{
			Name:     "scan",
			Model:    "m",
			Prompt:   Prompt{Base: "p.md"},
			Steering: Steering{Mode: SteeringScheduled},
			Output:   Output{Capture: OutputCaptureText},
			Fanout: &FanoutConfig{
				Over:  "scan.items",
				Child: FanoutChildConfig{Model: "m", Prompt: Prompt{Base: "c.md"}},
			},
		}},
	}
	err := Validate(&cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a preceding phase")
}

func TestValidate_RequiresName(t *testing.T) {
	cfg := AgentConfig{Strategy: "external_agent", Runner: "claude"}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "name is required")
}

func TestValidate_RejectsWhitespaceName(t *testing.T) {
	// A whitespace-only name survives a plain == "" check but produces the same
	// broken, ambiguous dispatch identity as an empty name, so Validate must
	// reject it too (matches the config.LoadWorkflow guard; see #109).
	cfg := AgentConfig{Name: "   ", Strategy: "external_agent", Runner: "claude"}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "name is required")
}

func TestValidatePhase_RejectsWhitespaceName(t *testing.T) {
	// A whitespace-only phase name keys the dup-name / fanout.over tables
	// ambiguously, so reject it like the top-level name (see #112). On pre-#112
	// code the whitespace name passed and the config validated clean.
	cfg := AgentConfig{
		Name: "x", Strategy: "external_agent", Runner: "claude",
		Phases: []Phase{{
			Name:     "   ",
			Model:    "claude-opus-4-6",
			Prompt:   Prompt{Base: "prompts/x.md"},
			Steering: Steering{Mode: SteeringScheduled},
			Output:   Output{Capture: OutputCaptureText},
		}},
	}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "name is required")
}

func TestValidateDeclaredToolList_RejectsWhitespaceName(t *testing.T) {
	// A whitespace-only declared-tool name is an ambiguous LLM-facing dispatch
	// identity; reject it (see #112). On pre-#112 code the whitespace name passed
	// the name check and the failure was "command is required" instead.
	err := validateDeclaredToolList("supervisor.tools", []DeclaredTool{{Name: "   "}})
	assert.ErrorContains(t, err, "name is required")
}

func TestValidate_RequiresStrategy(t *testing.T) {
	cfg := AgentConfig{Name: "x", Runner: "claude"}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "strategy")
}

func TestValidate_RequiresRunner(t *testing.T) {
	cfg := AgentConfig{Name: "x", Strategy: "external_agent"}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "runner")
}

func TestValidate_RejectsUnknownRunner(t *testing.T) {
	cfg := AgentConfig{
		Name: "x", Strategy: "external_agent", Runner: "clauode",
		Phases: []Phase{{
			Name:     "main",
			Model:    "claude-opus-4-6",
			Prompt:   Prompt{Base: "prompts/x.md"},
			Steering: Steering{Mode: SteeringScheduled},
			Output:   Output{Capture: OutputCaptureText},
		}},
	}
	err := Validate(&cfg)
	require.Error(t, err, "typo in runner name must fail at load time, not at dispatch")
	assert.ErrorContains(t, err, "clauode")
	assert.ErrorContains(t, err, "claude")
}

func TestValidate_AcceptsKnownRunners(t *testing.T) {
	for _, name := range KnownRunners() {
		t.Run(name, func(t *testing.T) {
			cfg := AgentConfig{
				Name: "x", Strategy: "external_agent", Runner: name,
				Phases: []Phase{{
					Name:     "main",
					Model:    "model",
					Prompt:   Prompt{Base: "prompts/x.md"},
					Steering: Steering{Mode: SteeringScheduled},
					Output:   Output{Capture: OutputCaptureText},
				}},
			}
			assert.NoError(t, Validate(&cfg))
		})
	}
}

func TestValidate_RequiresAtLeastOnePhase(t *testing.T) {
	cfg := AgentConfig{Name: "x", Strategy: "external_agent", Runner: "claude"}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "phase")
}

func TestValidate_PhaseRequiresModel(t *testing.T) {
	cfg := AgentConfig{
		Name: "x", Strategy: "external_agent", Runner: "claude",
		Phases: []Phase{{Name: "main"}},
	}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "model")
}

func TestValidate_DefaultsConcurrency(t *testing.T) {
	cfg := AgentConfig{
		Name: "x", Strategy: "external_agent", Runner: "claude",
		Phases: []Phase{{Name: "main", Model: "claude-sonnet-4-6",
			Steering: Steering{Mode: SteeringScheduled},
			Output:   Output{Capture: OutputCaptureText},
			Prompt:   Prompt{Base: "x.md"}}},
	}
	require.NoError(t, ApplyDefaults(&cfg))
	assert.Equal(t, 1, cfg.Concurrency.MaxConcurrent)
	assert.Equal(t, "skip", cfg.Concurrency.OnConflict)
}

func TestValidate_RejectsDuplicatePhaseNames(t *testing.T) {
	cfg := AgentConfig{
		Name: "x", Strategy: "external_agent", Runner: "claude",
		Phases: []Phase{
			{Name: "main", Model: "m", Prompt: Prompt{Base: "a.md"},
				Steering: Steering{Mode: SteeringScheduled}, Output: Output{Capture: OutputCaptureText}},
			{Name: "main", Model: "m", Prompt: Prompt{Base: "b.md"},
				Steering: Steering{Mode: SteeringScheduled}, Output: Output{Capture: OutputCaptureText}},
		},
	}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "duplicate name")
}

func TestApplyDefaults_RunnerFlagsDefaultTrue(t *testing.T) {
	cfg := AgentConfig{
		Name: "x", Strategy: "external_agent", Runner: "claude",
		Phases: []Phase{{Name: "main", Model: "claude-sonnet-4-6",
			Steering: Steering{Mode: SteeringScheduled},
			Output:   Output{Capture: OutputCaptureText},
			Prompt:   Prompt{Base: "x.md"}}},
	}
	require.NoError(t, ApplyDefaults(&cfg))
	rf := cfg.Phases[0].RunnerFlags
	require.NotNil(t, rf.Bare)
	assert.True(t, *rf.Bare)
	require.NotNil(t, rf.StrictMCPConfig)
	assert.True(t, *rf.StrictMCPConfig)
	require.NotNil(t, rf.NoSessionPersistence)
	assert.True(t, *rf.NoSessionPersistence)
}

func TestValidate_MCPServerHTTPRequiresURL(t *testing.T) {
	cfg := validConfig()
	cfg.Phases[0].Tools.MCP.Servers = map[string]MCPServerConfig{
		"bad": {Type: "http"},
	}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "url is required")
}

func TestValidate_MCPServerStdioRequiresCommand(t *testing.T) {
	cfg := validConfig()
	cfg.Phases[0].Tools.MCP.Servers = map[string]MCPServerConfig{
		"bad": {Type: "stdio"},
	}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "command is required")
}

func TestValidate_MCPServerEmptyRejectsNoURLNoCommand(t *testing.T) {
	cfg := validConfig()
	cfg.Phases[0].Tools.MCP.Servers = map[string]MCPServerConfig{
		"bad": {},
	}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "must set either url")
}

func TestValidate_MCPServerUnknownType(t *testing.T) {
	cfg := validConfig()
	cfg.Phases[0].Tools.MCP.Servers = map[string]MCPServerConfig{
		"bad": {Type: "grpc"},
	}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "unknown type")
}

func TestValidate_MCPServerHTTPValid(t *testing.T) {
	cfg := validConfig()
	cfg.Phases[0].Tools.MCP.Servers = map[string]MCPServerConfig{
		"ok": {Type: "http", URL: "http://localhost:8890/mcp/"},
	}
	assert.NoError(t, Validate(&cfg))
}

func TestValidate_MCPServerStdioValid(t *testing.T) {
	cfg := validConfig()
	cfg.Phases[0].Tools.MCP.Servers = map[string]MCPServerConfig{
		"ok": {Type: "stdio", Command: "/usr/bin/tool"},
	}
	assert.NoError(t, Validate(&cfg))
}

func TestValidate_MCPServerInferredFromURL(t *testing.T) {
	cfg := validConfig()
	cfg.Phases[0].Tools.MCP.Servers = map[string]MCPServerConfig{
		"ok": {URL: "http://localhost:8890/mcp/"},
	}
	assert.NoError(t, Validate(&cfg))
}

func TestValidate_MCPServerAmbiguousBothURLAndCommand(t *testing.T) {
	cfg := validConfig()
	cfg.Phases[0].Tools.MCP.Servers = map[string]MCPServerConfig{
		"bad": {URL: "http://localhost", Command: "/usr/bin/tool"},
	}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "set type")
}

func TestValidate_MCPServerInferredFromCommand(t *testing.T) {
	cfg := validConfig()
	cfg.Phases[0].Tools.MCP.Servers = map[string]MCPServerConfig{
		"ok": {Command: "/usr/bin/tool"},
	}
	assert.NoError(t, Validate(&cfg))
}

func validConfig() AgentConfig {
	return AgentConfig{
		Name: "x", Strategy: "external_agent", Runner: "claude",
		Phases: []Phase{{Name: "main", Model: "claude-sonnet-4-6",
			Steering: Steering{Mode: SteeringScheduled},
			Output:   Output{Capture: OutputCaptureText},
			Prompt:   Prompt{Base: "x.md"}}},
	}
}

func TestValidate_RejectsTopLevelBackend(t *testing.T) {
	t.Parallel()
	// backend is a sub-agent-only key; a top-level task that sets it is a
	// config defect (BackendOf would silently treat the whole task as a
	// backend it never dispatches through).
	cfg := validConfig()
	cfg.Backend = BackendNative
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "backend is only valid on supervisor sub-agents")
}

func TestValidate_FanoutRequiresOver(t *testing.T) {
	cfg := minimalConfig()
	cfg.Phases = append(cfg.Phases, Phase{
		Name: "fan", Model: "claude-opus-4-6",
		Prompt: Prompt{Base: "x.md"}, Steering: Steering{Mode: SteeringScheduled},
		Output: Output{Capture: OutputCaptureText},
		Fanout: &FanoutConfig{MaxParallel: 2, Child: minimalChildConfig()},
	})
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "fanout.over")
}

func TestValidate_FanoutRequiresChildModel(t *testing.T) {
	cfg := minimalConfig()
	cfg.Phases = append(cfg.Phases, Phase{
		Name: "fan", Model: "claude-opus-4-6",
		Prompt: Prompt{Base: "x.md"}, Steering: Steering{Mode: SteeringScheduled},
		Output: Output{Capture: OutputCaptureText},
		Fanout: &FanoutConfig{Over: "main.items", MaxParallel: 2,
			Child: FanoutChildConfig{Prompt: Prompt{Base: "c.md"}}},
	})
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "child.model")
}

func TestValidate_FanoutRequiresChildPrompt(t *testing.T) {
	cfg := minimalConfig()
	cfg.Phases = append(cfg.Phases, Phase{
		Name: "fan", Model: "claude-opus-4-6",
		Prompt: Prompt{Base: "x.md"}, Steering: Steering{Mode: SteeringScheduled},
		Output: Output{Capture: OutputCaptureText},
		Fanout: &FanoutConfig{Over: "main.items", MaxParallel: 2,
			Child: FanoutChildConfig{Model: "claude-opus-4-6"}},
	})
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "child.prompt.base")
}

func TestValidate_FanoutMaxParallelDefaultsTo1(t *testing.T) {
	cfg := minimalConfig()
	cfg.Phases = append(cfg.Phases, Phase{
		Name: "fan", Model: "claude-opus-4-6",
		Prompt: Prompt{Base: "x.md"}, Steering: Steering{Mode: SteeringScheduled},
		Output: Output{Capture: OutputCaptureText},
		Fanout: &FanoutConfig{Over: "main.items", Child: minimalChildConfig()},
	})
	require.NoError(t, ApplyDefaults(&cfg))
	assert.Equal(t, 1, cfg.Phases[1].Fanout.MaxParallel)
}

func TestValidate_FanoutOverMustReferenceEarlierPhase(t *testing.T) {
	cfg := minimalConfig()
	cfg.Phases = append(cfg.Phases, Phase{
		Name: "fan", Model: "claude-opus-4-6",
		Prompt: Prompt{Base: "x.md"}, Steering: Steering{Mode: SteeringScheduled},
		Output: Output{Capture: OutputCaptureText},
		Fanout: &FanoutConfig{Over: "nonexistent.items", MaxParallel: 2,
			Child: minimalChildConfig()},
	})
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "nonexistent")
}

func TestValidate_FanoutValid(t *testing.T) {
	cfg := minimalConfig()
	cfg.Phases = append(cfg.Phases, Phase{
		Name: "fan", Model: "claude-opus-4-6",
		Prompt: Prompt{Base: "x.md"}, Steering: Steering{Mode: SteeringScheduled},
		Output: Output{Capture: OutputCaptureText},
		Fanout: &FanoutConfig{Over: "main.items", MaxParallel: 4,
			Child: minimalChildConfig()},
	})
	require.NoError(t, ApplyDefaults(&cfg))
	require.NoError(t, Validate(&cfg))
}

func minimalConfig() AgentConfig {
	return AgentConfig{
		Name: "test", Strategy: "external_agent", Runner: "claude",
		Phases: []Phase{{
			Name: "main", Model: "claude-sonnet-4-6",
			Prompt: Prompt{Base: "p.md"}, Steering: Steering{Mode: SteeringScheduled},
			Output: Output{Capture: OutputCaptureText},
		}},
	}
}

func minimalChildConfig() FanoutChildConfig {
	return FanoutChildConfig{
		Model: "claude-opus-4-6", Prompt: Prompt{Base: "child.md"},
		Steering: Steering{Mode: SteeringScheduled},
		Output:   Output{Capture: OutputCaptureText},
	}
}

func TestLoadFromDir_LoadsSentryTriageConfig(t *testing.T) {
	cfgs, err := LoadFromDir("../../workflows/agents")
	require.NoError(t, err)

	var triage *AgentConfig
	for i := range cfgs {
		if cfgs[i].Name == "sentry-triage" {
			triage = &cfgs[i]
			break
		}
	}
	require.NotNil(t, triage, "sentry-triage config must be present")
	assert.Equal(t, "claude", triage.Runner)
	assert.NotEmpty(t, triage.Prefilter.Command)
	require.Len(t, triage.Phases, 1)
	assert.Equal(t, "main", triage.Phases[0].Name)
	assert.Equal(t, SteeringScheduled, triage.Phases[0].Steering.Mode)
}

func TestLoadFromDir_LoadsIssueUpdateConfig(t *testing.T) {
	cfgs, err := LoadFromDir("../../workflows/agents")
	require.NoError(t, err)

	var iu *AgentConfig
	for i := range cfgs {
		if cfgs[i].Name == "issue-update" {
			iu = &cfgs[i]
			break
		}
	}
	require.NotNil(t, iu, "issue-update config must be present")
	require.Len(t, iu.Phases, 1)
	assert.Equal(t, OutputCaptureStructured, iu.Phases[0].Output.Capture)
	assert.NotNil(t, iu.Phases[0].Output.Schema, "structured output requires a schema")
}

func TestApplyDefaults_RunnerFlagsExplicitFalsePreserved(t *testing.T) {
	f := false
	cfg := AgentConfig{
		Name: "x", Strategy: "external_agent", Runner: "claude",
		Phases: []Phase{{Name: "main", Model: "claude-sonnet-4-6",
			RunnerFlags: RunnerFlags{Bare: &f, StrictMCPConfig: &f, NoSessionPersistence: &f},
			Steering:    Steering{Mode: SteeringScheduled},
			Output:      Output{Capture: OutputCaptureText},
			Prompt:      Prompt{Base: "x.md"}}},
	}
	require.NoError(t, ApplyDefaults(&cfg))
	rf := cfg.Phases[0].RunnerFlags
	assert.False(t, *rf.Bare)
	assert.False(t, *rf.StrictMCPConfig)
	assert.False(t, *rf.NoSessionPersistence)
}

func validQueueMonitorConfig() AgentConfig {
	return AgentConfig{
		Name:      "x",
		Strategy:  "queue_monitor",
		Schedule:  Schedule{Cron: "*/5 * * * *"},
		Prefilter: Prefilter{Type: "command", Command: "emit-candidates"},
	}
}

func TestValidate_QueueMonitorValid(t *testing.T) {
	cfg := validQueueMonitorConfig()
	assert.NoError(t, Validate(&cfg))
}

func TestValidate_QueueMonitorRequiresPrefilter(t *testing.T) {
	cfg := validQueueMonitorConfig()
	cfg.Prefilter = Prefilter{}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "prefilter")
}

func TestValidate_QueueMonitorRequiresCron(t *testing.T) {
	cfg := validQueueMonitorConfig()
	cfg.Schedule = Schedule{}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "schedule.cron")
}

func TestValidate_QueueMonitorDoesNotRequireRunnerOrPhases(t *testing.T) {
	cfg := validQueueMonitorConfig()
	require.Empty(t, cfg.Runner)
	require.Empty(t, cfg.Phases)
	assert.NoError(t, Validate(&cfg))
}

func TestValidate_RejectsUnknownStrategy(t *testing.T) {
	cfg := AgentConfig{Name: "x", Strategy: "fanout_supreme"}
	err := Validate(&cfg)
	assert.ErrorContains(t, err, "fanout_supreme")
}

func TestApplyDefaults_QueueMonitorFillsMonitorDefaults(t *testing.T) {
	cfg := validQueueMonitorConfig()
	require.NoError(t, ApplyDefaults(&cfg))
	assert.Equal(t, 5, cfg.Monitor.MaxParallelCases)
	assert.Equal(t, 20, cfg.Monitor.MaxNewCasesPerPass)
	assert.Equal(t, 3, cfg.Monitor.MaxCaseAttempts)
}

func TestApplyDefaults_QueueMonitorRespectsExplicitValues(t *testing.T) {
	cfg := validQueueMonitorConfig()
	cfg.Monitor = Monitor{MaxParallelCases: 1, MaxNewCasesPerPass: 2, MaxCaseAttempts: 4}
	require.NoError(t, ApplyDefaults(&cfg))
	assert.Equal(t, 1, cfg.Monitor.MaxParallelCases)
	assert.Equal(t, 2, cfg.Monitor.MaxNewCasesPerPass)
	assert.Equal(t, 4, cfg.Monitor.MaxCaseAttempts)
}

func nativeSubAgentConfig(mut func(na *NativeAgent)) AgentConfig {
	na := NativeAgent{
		LoopConfig: LoopConfig{
			Prompt: Prompt{Base: "prompts/agents/x.md"},
		},
		Output: Output{Schema: map[string]any{
			"type":     "object",
			"required": []any{"summary"},
			"properties": map[string]any{
				"summary": map[string]any{"type": "string"},
			},
		}},
	}
	if mut != nil {
		mut(&na)
	}
	return AgentConfig{
		Name:      "q",
		Strategy:  StrategyQueueMonitor,
		Schedule:  Schedule{Cron: "0 7 * * *"},
		Prefilter: Prefilter{Type: "command", Command: "emit-candidates"},
		Monitor:   Monitor{MaxParallelCases: 1, MaxNewCasesPerPass: 1, MaxCaseAttempts: 1},
		Supervisor: Supervisor{
			LoopConfig: LoopConfig{
				Prompt: Prompt{Base: "prompts/agents/sup.md"},
			},
			SubAgents: map[string]AgentConfig{
				"analysis": {Backend: BackendNative, Native: na},
			},
		},
	}
}

func TestValidate_NativeSubAgent_OK(t *testing.T) {
	t.Parallel()
	cfg := nativeSubAgentConfig(nil)
	if err := Validate(&cfg); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidate_NativeSubAgent_MissingPromptBase(t *testing.T) {
	t.Parallel()
	cfg := nativeSubAgentConfig(func(na *NativeAgent) { na.Prompt.Base = "" })
	assert.ErrorContains(t, Validate(&cfg), "native.prompt.base")
}

func TestValidate_NativeSubAgent_MissingOutputSchema(t *testing.T) {
	t.Parallel()
	cfg := nativeSubAgentConfig(func(na *NativeAgent) { na.Output = Output{} })
	assert.ErrorContains(t, Validate(&cfg), "native.output.schema")
}

func TestValidate_NativeSubAgent_ToolNameCollidesWithTerminals(t *testing.T) {
	t.Parallel()
	for _, reserved := range []string{ReservedToolSubmitResult, ReservedToolReportFailure} {
		cfg := nativeSubAgentConfig(func(na *NativeAgent) {
			na.Tools = []DeclaredTool{{Name: reserved, Command: []string{"echo", "--", "{x}"}}}
		})
		assert.ErrorContains(t, Validate(&cfg), "collides with a reserved built-in tool")
	}
}

func TestValidate_NativeSubAgent_ToolMissingTerminator(t *testing.T) {
	t.Parallel()
	cfg := nativeSubAgentConfig(func(na *NativeAgent) {
		na.Tools = []DeclaredTool{{Name: "t", Command: []string{"echo", "{x}"}}}
	})
	assert.ErrorContains(t, Validate(&cfg), "flag terminator")
}

func TestValidate_NativeSubAgent_ToolNameEmpty(t *testing.T) {
	t.Parallel()
	cfg := nativeSubAgentConfig(func(na *NativeAgent) {
		na.Tools = []DeclaredTool{{Name: "", Command: []string{"echo", "--", "{x}"}}}
	})
	assert.ErrorContains(t, Validate(&cfg), "name is required")
}

func TestValidate_NativeSubAgent_ToolNameDuplicate(t *testing.T) {
	t.Parallel()
	cfg := nativeSubAgentConfig(func(na *NativeAgent) {
		na.Tools = []DeclaredTool{
			{Name: "t", Command: []string{"echo", "--", "a"}},
			{Name: "t", Command: []string{"echo", "--", "b"}},
		}
	})
	assert.ErrorContains(t, Validate(&cfg), "duplicate name")
}

func TestValidate_NativeSubAgent_UnknownBackend(t *testing.T) {
	t.Parallel()
	cfg := nativeSubAgentConfig(nil)
	sub := cfg.Supervisor.SubAgents["analysis"]
	sub.Backend = "weird"
	cfg.Supervisor.SubAgents["analysis"] = sub
	assert.ErrorContains(t, Validate(&cfg), "backend must be")
}

func TestValidate_CLISubAgent_StillValidated(t *testing.T) {
	t.Parallel()
	// A cli sub-agent with no runner must still be rejected by validateExternalAgent.
	cfg := nativeSubAgentConfig(nil)
	cfg.Supervisor.SubAgents = map[string]AgentConfig{
		"action": {Backend: BackendCLI},
	}
	assert.ErrorContains(t, Validate(&cfg), "runner is required")
}

func TestApplyDefaults_RecursesIntoSubAgentPhases(t *testing.T) {
	// A cli sub-agent phase that omits steering.mode/output.capture must get
	// the same defaults a top-level phase gets, so Validate accepts it instead
	// of rejecting it with a confusing `invalid steering.mode ""`.
	cfg := AgentConfig{
		Name:      "sup",
		Strategy:  StrategyQueueMonitor,
		Schedule:  Schedule{Cron: "0 7 * * *"},
		Prefilter: Prefilter{Type: "command", Command: "emit"},
		Supervisor: Supervisor{
			LoopConfig: LoopConfig{Prompt: Prompt{Base: "prompts/sup.md"}},
			SubAgents: map[string]AgentConfig{
				"investigate": {
					Runner: testAgentRunner,
					Phases: []Phase{{
						Name:   testAgentPhaseMain,
						Model:  testAgentModel,
						Prompt: Prompt{Base: "prompts/sub.md"},
						// Steering.Mode and Output.Capture deliberately omitted.
					}},
				},
			},
		},
	}
	require.NoError(t, ApplyDefaults(&cfg))

	sub := cfg.Supervisor.SubAgents["investigate"]
	assert.Equal(t, SteeringScheduled, sub.Phases[0].Steering.Mode)
	assert.Equal(t, OutputCaptureText, sub.Phases[0].Output.Capture)
	// Runner-flag defaults recurse too.
	require.NotNil(t, sub.Phases[0].RunnerFlags.Bare)
	assert.True(t, *sub.Phases[0].RunnerFlags.Bare)

	// The fully-defaulted sub-agent now validates.
	require.NoError(t, Validate(&cfg))
}
