package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/llm"
)

const (
	testCaseToolsRunner    = "claude"
	testCaseToolsPropNote  = "note"
	testCaseToolsSubAgent1 = "investigate"
	testCaseToolsSubAgent2 = "remediate"
)

func TestSupervisorToolDeclarations_BuiltinsAlwaysPresent(t *testing.T) {
	decls := supervisorToolDeclarations(agentcfg.Supervisor{})
	require.Len(t, decls, 2, "record_outcome and spawn_agent must always be declared")
	assert.Equal(t, agentcfg.ReservedToolRecordOutcome, decls[0].Name)
	assert.Equal(t, agentcfg.ReservedToolSpawnAgent, decls[1].Name)

	spawnKind := decls[1].Parameters.Properties[argSpawnKind]
	require.NotNil(t, spawnKind)
	assert.Empty(t, spawnKind.Enum, "no sub-agents configured -> empty enum")
}

func TestSupervisorToolDeclarations_RecordOutcomeSchema(t *testing.T) {
	decls := supervisorToolDeclarations(agentcfg.Supervisor{})
	recordOutcome := decls[0]
	assert.Equal(t, agentcfg.ReservedToolRecordOutcome, recordOutcome.Name)
	require.NotNil(t, recordOutcome.Parameters)
	assert.Equal(t, llm.TypeObject, recordOutcome.Parameters.Type)

	status := recordOutcome.Parameters.Properties[argRecordStatus]
	require.NotNil(t, status)
	assert.Equal(t, llm.TypeString, status.Type)
	assert.ElementsMatch(t, recordOutcomeStatuses, status.Enum)
	assert.Contains(t, recordOutcome.Parameters.Required, argRecordStatus)

	outcome := recordOutcome.Parameters.Properties[argRecordOutcome]
	require.NotNil(t, outcome)
	assert.NotContains(t, recordOutcome.Parameters.Required, argRecordOutcome)
}

func TestSupervisorToolDeclarations_FullSupervisor(t *testing.T) {
	sup := agentcfg.Supervisor{
		LoopConfig: agentcfg.LoopConfig{
			Tools: []agentcfg.DeclaredTool{
				{
					Name:        "zzz_last",
					Description: "runs last alphabetically",
					Command:     []string{"zzz", "--"},
					ArgsSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							testCaseToolsPropNote: map[string]any{"type": "string"},
						},
						"required": []any{testCaseToolsPropNote},
					},
				},
				{
					Name:        "aaa_first",
					Description: "runs first alphabetically",
					Command:     []string{"aaa", "--"},
				},
			},
		},
		SubAgents: map[string]agentcfg.AgentConfig{
			testCaseToolsSubAgent1: {Runner: testCaseToolsRunner},
			testCaseToolsSubAgent2: {Runner: testCaseToolsRunner},
		},
	}

	decls := supervisorToolDeclarations(sup)
	require.Len(t, decls, 4)

	// Deterministic order: record_outcome, spawn_agent, then declared tools
	// sorted by name.
	names := make([]string, len(decls))
	for i, d := range decls {
		names[i] = d.Name
	}
	assert.Equal(t, []string{
		agentcfg.ReservedToolRecordOutcome,
		agentcfg.ReservedToolSpawnAgent,
		"aaa_first",
		"zzz_last",
	}, names)

	spawnKind := decls[1].Parameters.Properties[argSpawnKind]
	require.NotNil(t, spawnKind)
	assert.Equal(t, []string{testCaseToolsSubAgent1, testCaseToolsSubAgent2}, spawnKind.Enum, "sub-agent kinds must be sorted")

	zzz := decls[3]
	assert.Equal(t, "runs last alphabetically", zzz.Description)
	require.NotNil(t, zzz.Parameters)
	assert.Equal(t, llm.TypeObject, zzz.Parameters.Type)
	note := zzz.Parameters.Properties[testCaseToolsPropNote]
	require.NotNil(t, note)
	assert.Equal(t, llm.TypeString, note.Type)
	assert.Equal(t, []string{testCaseToolsPropNote}, zzz.Parameters.Required)

	aaa := decls[2]
	assert.Equal(t, "runs first alphabetically", aaa.Description)
	require.NotNil(t, aaa.Parameters, "nil ArgsSchema must still produce an object schema")
	assert.Equal(t, llm.TypeObject, aaa.Parameters.Type)
}

func TestSupervisorToolDeclarations_Deterministic(t *testing.T) {
	sup := agentcfg.Supervisor{
		LoopConfig: agentcfg.LoopConfig{
			Tools: []agentcfg.DeclaredTool{
				{Name: "c_tool", Command: []string{"c", "--"}},
				{Name: "a_tool", Command: []string{"a", "--"}},
				{Name: "b_tool", Command: []string{"b", "--"}},
			},
		},
	}
	first := supervisorToolDeclarations(sup)
	second := supervisorToolDeclarations(sup)
	require.Len(t, first, len(second))
	for i := range first {
		assert.Equal(t, first[i].Name, second[i].Name)
	}
}

func TestNativeSubAgentToolDeclarations(t *testing.T) {
	t.Parallel()
	na := agentcfg.NativeAgent{
		LoopConfig: agentcfg.LoopConfig{
			Tools: []agentcfg.DeclaredTool{
				{Name: "beta", Command: []string{"x", "--"}},
				{Name: "alpha", Command: []string{"x", "--"}},
			},
		},
		Output: agentcfg.Output{Schema: map[string]any{
			"type":     "object",
			"required": []any{"summary"},
			"properties": map[string]any{
				"summary": map[string]any{"type": "string"},
			},
		}},
	}
	decls := nativeSubAgentToolDeclarations(na)
	// submit_result and report_failure first, then declared tools sorted by name.
	if len(decls) != 4 {
		t.Fatalf("len(decls) = %d, want 4", len(decls))
	}
	if decls[0].Name != agentcfg.ReservedToolSubmitResult || decls[1].Name != agentcfg.ReservedToolReportFailure {
		t.Fatalf("built-ins = %q,%q, want submit_result,report_failure", decls[0].Name, decls[1].Name)
	}
	if decls[2].Name != "alpha" || decls[3].Name != "beta" {
		t.Fatalf("declared order = %q,%q, want alpha,beta", decls[2].Name, decls[3].Name)
	}
	// submit_result's parameters carry the declared output schema.
	if decls[0].Parameters == nil || decls[0].Parameters.Properties["summary"] == nil {
		t.Fatalf("submit_result params missing summary property: %+v", decls[0].Parameters)
	}
	if len(decls[0].Parameters.Required) != 1 || decls[0].Parameters.Required[0] != "summary" {
		t.Fatalf("submit_result required = %v, want [summary]", decls[0].Parameters.Required)
	}
	// report_failure requires a reason string.
	if decls[1].Parameters == nil || decls[1].Parameters.Properties[argFailReason] == nil {
		t.Fatalf("report_failure params missing reason: %+v", decls[1].Parameters)
	}
	assert.Contains(t, decls[1].Parameters.Required, argFailReason)
}

func TestMissingRequiredFields(t *testing.T) {
	t.Parallel()
	schema := map[string]any{"type": "object", "required": []any{"a", "b"}}
	if gaps := missingRequiredFields(schema, map[string]any{"a": 1, "b": 2}); len(gaps) != 0 {
		t.Fatalf("gaps = %v, want none", gaps)
	}
	gaps := missingRequiredFields(schema, map[string]any{"a": 1, "b": nil})
	if len(gaps) != 1 || gaps[0] != "b" {
		t.Fatalf("gaps = %v, want [b] (present-but-nil counts as missing)", gaps)
	}
	gaps = missingRequiredFields(schema, map[string]any{"a": 1})
	if len(gaps) != 1 || gaps[0] != "b" {
		t.Fatalf("gaps = %v, want [b]", gaps)
	}
	if gaps := missingRequiredFields(nil, map[string]any{}); gaps != nil {
		t.Fatalf("gaps = %v, want nil for nil schema", gaps)
	}
}

func TestSubmitResultTypeMismatches(t *testing.T) {
	t.Parallel()
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary": map[string]any{"type": "string"},
			"details": map[string]any{"type": "object"},
			"count":   map[string]any{"type": "integer"},
			"flags":   map[string]any{"type": "array"},
		},
	}
	ok := map[string]any{
		"summary": "fine",
		"details": map[string]any{"k": "v"},
		"count":   float64(3), // JSON numbers decode as float64; integer stays lenient
		"flags":   []any{"a"},
	}
	if m := submitResultTypeMismatches(schema, ok); len(m) != 0 {
		t.Fatalf("mismatches = %v, want none", m)
	}
	bad := map[string]any{
		"summary": []any{"not", "a", "string"},
		"details": "not an object",
	}
	m := submitResultTypeMismatches(schema, bad)
	// Sorted property order: details before summary.
	if len(m) != 2 || m[0] != "details: expected object" || m[1] != "summary: expected string" {
		t.Fatalf("mismatches = %v, want [details: expected object, summary: expected string]", m)
	}
	// Numeric leniency spans int/float representations but does not accept a
	// non-numeric value: a string where an integer is declared is a mismatch.
	m = submitResultTypeMismatches(schema, map[string]any{"count": "abc"})
	if len(m) != 1 || m[0] != "count: expected integer" {
		t.Fatalf("mismatches = %v, want [count: expected integer]", m)
	}
	// Absent keys are missingRequiredFields' job, not a type mismatch.
	if m := submitResultTypeMismatches(schema, map[string]any{}); len(m) != 0 {
		t.Fatalf("mismatches = %v, want none for absent keys", m)
	}
	if m := submitResultTypeMismatches(nil, map[string]any{"x": 1}); m != nil {
		t.Fatalf("mismatches = %v, want nil for nil schema", m)
	}
}
