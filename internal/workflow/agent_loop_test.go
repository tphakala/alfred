package workflow

import (
	"testing"

	"github.com/tphakala/alfred/internal/agentcfg"
)

func TestClassifyToolCalls_NativeShape(t *testing.T) {
	t.Parallel()
	declared := map[string]agentcfg.DeclaredTool{"lookup": {Name: "lookup"}}
	calls := []LLMToolCall{
		{CallID: "t1-c0", Name: "lookup"},
		{CallID: "t1-c1", Name: "spawn_agent"}, // native has no spawn tool: unknown
		{CallID: "t1-c2", Name: "submit_result"},
		{CallID: "t1-c3", Name: "report_failure"}, // second terminal-name call: unknown
		{CallID: "t1-c4", Name: "nope"},
	}
	terminal, spawns, declaredCalls, unknown := classifyToolCalls(calls,
		[]string{"submit_result", "report_failure"}, "", declared)
	if terminal == nil || terminal.CallID != "t1-c2" {
		t.Fatalf("terminal = %+v, want first terminal-name call (t1-c2)", terminal)
	}
	if len(spawns) != 0 {
		t.Fatalf("spawns = %d, want 0 (no spawn tool)", len(spawns))
	}
	if len(declaredCalls) != 1 || declaredCalls[0].CallID != "t1-c0" {
		t.Fatalf("declared = %+v, want [t1-c0]", declaredCalls)
	}
	if len(unknown) != 3 { // spawn_agent, report_failure (dup terminal), nope
		t.Fatalf("unknown = %d, want 3", len(unknown))
	}
}

func TestClassifyToolCalls_SupervisorShape(t *testing.T) {
	t.Parallel()
	declared := map[string]agentcfg.DeclaredTool{"comment": {Name: "comment"}}
	calls := []LLMToolCall{
		{CallID: "t1-c0", Name: "spawn_agent"},
		{CallID: "t1-c1", Name: "comment"},
		{CallID: "t1-c2", Name: "record_outcome"},
	}
	terminal, spawns, declaredCalls, unknown := classifyToolCalls(calls,
		[]string{"record_outcome"}, "spawn_agent", declared)
	if terminal == nil || terminal.CallID != "t1-c2" {
		t.Fatalf("terminal = %+v, want record_outcome", terminal)
	}
	if len(spawns) != 1 || len(declaredCalls) != 1 || len(unknown) != 0 {
		t.Fatalf("spawns=%d declared=%d unknown=%d, want 1/1/0", len(spawns), len(declaredCalls), len(unknown))
	}
}

func TestLoopRoundMessages_EmptyRoundStillPersistsModelMessage(t *testing.T) {
	t.Parallel()
	// Preserves CaseWorkflow's pre-extraction behavior: a round with no tool
	// calls persists exactly one model message even when the text is empty.
	msgs := loopRoundMessages("", false, nil)
	if len(msgs) != 1 || msgs[0].Content != "" {
		t.Fatalf("msgs = %+v, want exactly one empty model message", msgs)
	}
	// With tool messages, defer to buildRoundMessages (drops empty text).
	toolMsgs := buildToolMessages(LLMToolCall{CallID: "c", Name: "x"}, &ExecToolResult{CallID: "c", Name: "x", Result: "r"})
	msgs = loopRoundMessages("", true, toolMsgs)
	if len(msgs) != len(toolMsgs) {
		t.Fatalf("len = %d, want %d (no empty model message when tools ran)", len(msgs), len(toolMsgs))
	}
	// Terminal-only round: calls existed but none produced tool messages (the
	// sole call was the terminal tool). It must persist nothing, so it does not
	// write a spurious empty model message.
	msgs = loopRoundMessages("", true, nil)
	if len(msgs) != 0 {
		t.Fatalf("len = %d, want 0 (terminal-only round persists nothing)", len(msgs))
	}
}
