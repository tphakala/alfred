package agentcfg

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test literals shared across supervisor_test.go and strip_test.go (same
// package), factored out to keep goconst happy.
const (
	testAgentModel       = "claude-opus-4-6"
	testAgentRunner      = "claude"
	testAgentPhaseMain   = "main"
	testToolCmdLookup    = "lookup"
	testPlaceholderQuery = "{query}"
	testSubAgentKind     = "investigate"
)

// queryArgsSchema is a minimal args_schema declaring a single required "query"
// string property, matching testPlaceholderQuery ("{query}"). Tools whose
// command references {query} need it so the placeholder cross-check accepts
// them: the property must exist, be a schema object, and be required.
func queryArgsSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"query": map[string]any{"type": "string"}},
		"required":   []any{"query"},
	}
}

// validSupervisorSubAgent is a minimal structurally-valid sub-agent config
// reused across the table below.
func validSupervisorSubAgent() AgentConfig {
	return AgentConfig{
		Runner: testAgentRunner,
		Phases: []Phase{{
			Name:     testAgentPhaseMain,
			Model:    testAgentModel,
			Prompt:   Prompt{Base: "prompts/sub.md"},
			Steering: Steering{Mode: SteeringScheduled},
			Output:   Output{Capture: OutputCaptureText},
		}},
	}
}

func TestValidate_Supervisor(t *testing.T) {
	tests := []struct {
		name       string
		supervisor Supervisor
		wantErr    string // substring; empty means no error expected
	}{
		{
			name: "valid supervisor with tool and sub-agent",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Model: testAgentModel,
					Tools: []DeclaredTool{{
						Name:       "look_up_thing",
						ArgsSchema: queryArgsSchema(),
						Command:    []string{testToolCmdLookup, "--", testPlaceholderQuery},
					}},
				},
				SubAgents: map[string]AgentConfig{
					testSubAgentKind: validSupervisorSubAgent(),
				},
			},
		},
		{
			name: "missing tool name",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{Command: []string{testToolCmdLookup, "--"}}},
				},
			},
			wantErr: "name is required",
		},
		{
			name: "duplicate tool name",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{
						{Name: "dup", Command: []string{"a", "--"}},
						{Name: "dup", Command: []string{"b", "--"}},
					},
				},
			},
			wantErr: "duplicate name",
		},
		{
			name: "reserved tool name spawn_agent",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{Name: ReservedToolSpawnAgent, Command: []string{"a", "--"}}},
				},
			},
			wantErr: "reserved",
		},
		{
			name: "reserved tool name record_outcome",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{Name: ReservedToolRecordOutcome, Command: []string{"a", "--"}}},
				},
			},
			wantErr: "reserved",
		},
		{
			name: "command missing flag terminator",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{Name: "no_terminator", Command: []string{testToolCmdLookup, testPlaceholderQuery}}},
				},
			},
			wantErr: "\"--\"",
		},
		{
			name: "empty command",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{Name: "empty_cmd", Command: nil}},
				},
			},
			wantErr: "command is required",
		},
		{
			name: "embedded placeholder prefix",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{Name: "embedded1", Command: []string{"pre-{query}", "--"}}},
				},
			},
			wantErr: "embedded placeholder",
		},
		{
			name: "embedded placeholder suffix",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{Name: "embedded2", Command: []string{"{query}-suf", "--"}}},
				},
			},
			wantErr: "embedded placeholder",
		},
		{
			name: "command[0] is a whole-element placeholder",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{Name: "placeholder_executable", Command: []string{testPlaceholderQuery, "--"}}},
				},
			},
			wantErr: "must be a literal, not a placeholder",
		},
		{
			name: "placeholder without matching args_schema property",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{
						Name:       "typo",
						ArgsSchema: queryArgsSchema(),
						Command:    []string{testToolCmdLookup, "--", "{qeury}"},
					}},
				},
			},
			wantErr: "args_schema",
		},
		{
			name: "placeholder with no args_schema declared",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{
						Name:    "no_schema",
						Command: []string{testToolCmdLookup, "--", testPlaceholderQuery},
					}},
				},
			},
			wantErr: "args_schema",
		},
		{
			name: "placeholder maps to a non-object args_schema property",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{
						Name:       "malformed_prop",
						ArgsSchema: map[string]any{"properties": map[string]any{"query": "string"}},
						Command:    []string{testToolCmdLookup, "--", testPlaceholderQuery},
					}},
				},
			},
			wantErr: "not a schema object",
		},
		{
			name: "placeholder maps to an optional args_schema property (no required list)",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{
						Name: "optional_prop",
						ArgsSchema: map[string]any{
							"type":       "object",
							"properties": map[string]any{"query": map[string]any{"type": "string"}},
						},
						Command: []string{testToolCmdLookup, "--", testPlaceholderQuery},
					}},
				},
			},
			wantErr: "not listed in args_schema.required",
		},
		{
			name: "placeholder property absent from a non-empty required list",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{
						Name: "other_required",
						ArgsSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"query": map[string]any{"type": "string"},
								"other": map[string]any{"type": "string"},
							},
							"required": []any{"other"},
						},
						Command: []string{testToolCmdLookup, "--", testPlaceholderQuery},
					}},
				},
			},
			wantErr: "not listed in args_schema.required",
		},
		{
			name: "args_schema.required is not a list",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Tools: []DeclaredTool{{
						Name: "scalar_required",
						ArgsSchema: map[string]any{
							"type":       "object",
							"properties": map[string]any{"query": map[string]any{"type": "string"}},
							"required":   "query",
						},
						Command: []string{testToolCmdLookup, "--", testPlaceholderQuery},
					}},
				},
			},
			wantErr: "args_schema.required must be a list",
		},
		{
			name: "sub-agent missing runner",
			supervisor: Supervisor{
				SubAgents: map[string]AgentConfig{
					"broken": {Phases: []Phase{{
						Name: testAgentPhaseMain, Model: "m", Prompt: Prompt{Base: "b.md"},
						Steering: Steering{Mode: SteeringScheduled}, Output: Output{Capture: OutputCaptureText},
					}}},
				},
			},
			wantErr: "runner is required",
		},
		{
			name: "sub-agent no phases",
			supervisor: Supervisor{
				SubAgents: map[string]AgentConfig{
					"broken": {Runner: testAgentRunner},
				},
			},
			wantErr: "at least one phase",
		},
		{
			name: "zero guard limits are accepted",
			supervisor: Supervisor{
				LoopConfig: LoopConfig{
					Model:       testAgentModel,
					MaxRounds:   0,
					CostCapUSD:  0,
					MaxDuration: 0,
					Tools:       []DeclaredTool{{Name: "t", Command: []string{"a", "--"}}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := AgentConfig{
				Name:       "supervisor-test",
				Strategy:   StrategyQueueMonitor,
				Prefilter:  Prefilter{Command: "true"},
				Schedule:   Schedule{Cron: "*/5 * * * *"},
				Supervisor: tt.supervisor,
			}
			err := Validate(&cfg)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestIsWholeElementPlaceholder(t *testing.T) {
	tests := []struct {
		elem string
		want bool
	}{
		{"{name}", true},
		{"--", false},
		{"literal", false},
		{"pre-{name}", false},
		{"{name}-suf", false},
		{"{}", false},
		{"{a{b}}", false},
		{"{", false},
		{"}", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.elem, func(t *testing.T) {
			assert.Equal(t, tt.want, IsWholeElementPlaceholder(tt.elem))
		})
	}
}
