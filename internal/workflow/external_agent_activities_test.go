package workflow

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
	"github.com/tphakala/alfred/internal/agentcfg"
	"github.com/tphakala/alfred/internal/ctxbuild"
	"github.com/tphakala/alfred/internal/store"
)

func TestPreparePhaseInput_BaseOnly(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.md"), []byte("hello world"), 0o644))

	a := &ExternalAgentActivities{}
	got, err := a.PreparePhaseInput(t.Context(), PreparePhaseInputArgs{
		SessionID:  uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
		Phase:      agentcfg.Phase{Name: "main", Model: "claude-sonnet-4-6", Prompt: agentcfg.Prompt{Base: "base.md"}, Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled}},
		ConfigName: "test-config",
		Runner:     "claude",
		PromptDir:  dir,
	})
	require.NoError(t, err)
	assert.Equal(t, uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"), got.SessionID)
	assert.Equal(t, "claude", got.Runner)
	assert.Equal(t, "main", got.Phase)
	assert.Equal(t, "hello world", got.Config.Prompt)
	assert.Equal(t, "claude-sonnet-4-6", got.Config.Model)
}

func TestPreparePhaseInput_HeaderAndIncludes(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.md"), []byte("BASE"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "voice.md"), []byte("VOICE"), 0o644))

	a := &ExternalAgentActivities{}
	got, err := a.PreparePhaseInput(t.Context(), PreparePhaseInputArgs{
		SessionID: uuid.New(),
		Phase: agentcfg.Phase{
			Name:  "main",
			Model: "claude-sonnet-4-6",
			Prompt: agentcfg.Prompt{
				Base:     "base.md",
				Includes: []string{"voice.md"},
				Header:   "HEAD",
			},
			Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled},
		},
		ConfigName: "x",
		Runner:     "claude",
		PromptDir:  dir,
	})
	require.NoError(t, err)
	assert.Equal(t, "HEAD\nVOICE\nBASE", got.Config.Prompt)
}

func TestPreparePhaseInput_PhaseOutputsInjected(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.md"), []byte("BASE"), 0o644))

	a := &ExternalAgentActivities{}
	got, err := a.PreparePhaseInput(t.Context(), PreparePhaseInputArgs{
		SessionID: uuid.New(),
		Phase: agentcfg.Phase{
			Name:  "main",
			Model: "claude-sonnet-4-6",
			Prompt: agentcfg.Prompt{
				Base:   "base.md",
				Header: "Candidates: {{ .prefilter.candidates }}",
			},
			Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled},
		},
		ConfigName: "x",
		Runner:     "claude",
		// JSON-roundtripped form: Temporal DataConverter deserializes into
		// map[string]any, not the original struct types.
		PhaseOutputs: map[string]any{
			"prefilter": map[string]any{"candidates": "1,2,3"},
		},
		PromptDir: dir,
	})
	require.NoError(t, err)
	assert.Equal(t, "Candidates: 1,2,3\nBASE", got.Config.Prompt)
}

func TestPreparePhaseInput_FanoutItemAvailableInTemplate(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.md"), []byte("BASE"), 0o644))

	a := &ExternalAgentActivities{}
	got, err := a.PreparePhaseInput(t.Context(), PreparePhaseInputArgs{
		SessionID: uuid.New(),
		Phase: agentcfg.Phase{
			Name:  "per_issue-0",
			Model: "claude-opus-4-6",
			Prompt: agentcfg.Prompt{
				Base:   "base.md",
				Header: "Issue: {{ .item.issue_number }}",
			},
			Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled},
		},
		ConfigName: "x",
		Runner:     "claude",
		// JSON-roundtripped form matches what resolvePath produces from
		// ExecOutput.StructuredOut (numbers become float64).
		Item:      map[string]any{"issue_number": float64(42)},
		PromptDir: dir,
	})
	require.NoError(t, err)
	assert.Equal(t, "Issue: 42\nBASE", got.Config.Prompt,
		"fanout child prompts must be able to reference {{ .item.X }}")
}

func TestPreparePhaseInput_MissingPromptFileErrors(t *testing.T) {
	a := &ExternalAgentActivities{}
	_, err := a.PreparePhaseInput(t.Context(), PreparePhaseInputArgs{
		SessionID: uuid.New(),
		Phase: agentcfg.Phase{
			Name:     "main",
			Prompt:   agentcfg.Prompt{Base: "nonexistent.md"},
			Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled},
		},
		ConfigName: "x",
		Runner:     "claude",
		PromptDir:  t.TempDir(),
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "nonexistent.md")
}

func TestPreparePhaseInput_RunnerFlagsMapped(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.md"), []byte("go"), 0o644))

	a := &ExternalAgentActivities{}
	got, err := a.PreparePhaseInput(t.Context(), PreparePhaseInputArgs{
		SessionID: uuid.New(),
		Phase: agentcfg.Phase{
			Name:  "build",
			Model: "claude-opus-4-6",
			RunnerFlags: agentcfg.RunnerFlags{
				MaxTurns:     50,
				MaxBudgetUSD: 1.5,
			},
			Prompt:   agentcfg.Prompt{Base: "base.md"},
			Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled},
			Tools: agentcfg.Tools{
				Bash:    []string{"git", "go"},
				Builtin: []string{"Read", "Write"},
				MCP: agentcfg.MCP{Servers: map[string]agentcfg.MCPServerConfig{
					"hindsight": {
						Type:    "http",
						URL:     "http://localhost:8890/mcp/hindsight/",
						Command: "npx",
						Args:    []string{"-y", "@hindsight/mcp"},
						Headers: map[string]string{"X-Test": "val"},
						Env:     map[string]string{"TOKEN": "abc"},
					},
				}},
			},
			Output: agentcfg.Output{Capture: agentcfg.OutputCaptureStructured},
		},
		ConfigName: "x",
		Runner:     "claude",
		PromptDir:  dir,
	})
	require.NoError(t, err)
	assert.Equal(t, "x", got.ConfigName, "ConfigName must be wired through")
	assert.Equal(t, 50, got.Config.MaxTurns)
	assert.InEpsilon(t, 1.5, got.Config.MaxBudgetUSD, 1e-9)
	assert.Equal(t, runner.SteeringScheduled, got.Config.Steering)
	assert.Equal(t, []string{"git", "go"}, got.Config.Tools.Bash)
	assert.Equal(t, []string{"Read", "Write"}, got.Config.Tools.Builtin)
	require.Contains(t, got.Config.MCP.Servers, "hindsight")
	s := got.Config.MCP.Servers["hindsight"]
	assert.Equal(t, "http", s.Type)
	assert.Equal(t, "http://localhost:8890/mcp/hindsight/", s.URL)
	assert.Equal(t, "npx", s.Command)
	assert.Equal(t, []string{"-y", "@hindsight/mcp"}, s.Args)
	assert.Equal(t, "val", s.Headers["X-Test"])
	assert.Equal(t, "abc", s.Env["TOKEN"])
	assert.Equal(t, runner.OutputCaptureStructured, got.Config.OutputCapture)
}

func TestRunPreflight_Success(t *testing.T) {
	a := &ExternalAgentActivities{}
	err := a.RunPreflight(t.Context(), agentcfg.PreflightStep{
		Name:    "ok",
		Type:    "bash",
		Command: "exit 0",
	})
	assert.NoError(t, err)
}

func TestRunPreflight_NonZeroFails(t *testing.T) {
	a := &ExternalAgentActivities{}
	err := a.RunPreflight(t.Context(), agentcfg.PreflightStep{
		Name:    "fail",
		Type:    "bash",
		Command: "exit 2",
	})
	assert.ErrorContains(t, err, "exit status 2")
}

func TestRunPrefilter_ProceedTrue(t *testing.T) {
	a := &ExternalAgentActivities{}
	out, err := a.RunPrefilter(t.Context(), agentcfg.Prefilter{
		Type:    "command",
		Command: `printf '{"proceed":true,"data":{"foo":42}}'`,
	})
	require.NoError(t, err)
	assert.True(t, out.Proceed)
	assert.InEpsilon(t, float64(42), out.Data["foo"], 1e-9)
}

func TestRunPrefilter_ProceedFalse(t *testing.T) {
	a := &ExternalAgentActivities{}
	out, err := a.RunPrefilter(t.Context(), agentcfg.Prefilter{
		Type:    "command",
		Command: `printf '{"proceed":false}'`,
	})
	require.NoError(t, err)
	assert.False(t, out.Proceed)
}

func TestRunPrefilter_InvalidJSONFails(t *testing.T) {
	a := &ExternalAgentActivities{}
	_, err := a.RunPrefilter(t.Context(), agentcfg.Prefilter{
		Type:    "command",
		Command: `printf 'not json'`,
	})
	assert.ErrorContains(t, err, "parse")
}

func TestRunPrefilter_CandidatesParsed(t *testing.T) {
	a := &ExternalAgentActivities{}
	out, err := a.RunPrefilter(t.Context(), agentcfg.Prefilter{
		Type: "command",
		Command: `printf '{"candidates":[` +
			`{"key":"a","context":{"title":"first"}},` +
			`{"key":"b"}` +
			`]}'`,
	})
	require.NoError(t, err)
	require.Len(t, out.Candidates, 2)
	assert.Equal(t, "a", out.Candidates[0].Key)
	assert.JSONEq(t, `{"title":"first"}`, string(out.Candidates[0].Context))
	assert.Equal(t, "b", out.Candidates[1].Key)
	assert.Empty(t, out.Candidates[1].Context)
}

func TestRunPrefilter_NoCandidatesKeepsExistingProceedGate(t *testing.T) {
	a := &ExternalAgentActivities{}
	out, err := a.RunPrefilter(t.Context(), agentcfg.Prefilter{
		Type:    "command",
		Command: `printf '{"proceed":true,"data":{"foo":1}}'`,
	})
	require.NoError(t, err)
	assert.True(t, out.Proceed)
	assert.Empty(t, out.Candidates)
}

func TestRunCleanup_BestEffortIgnoresErrors(t *testing.T) {
	a := &ExternalAgentActivities{}
	err := a.RunCleanup(t.Context(), agentcfg.CleanupStep{
		Bash: "exit 1",
	})
	assert.NoError(t, err, "cleanup is best-effort and never returns errors")
}

// captureSessionCreator records sessions and messages passed by CreateAgentSession
// so tests can assert how the activity populates store fields.
type captureSessionCreator struct {
	mu       sync.Mutex
	calls    []store.Session
	messages []store.Message
}

func (c *captureSessionCreator) CreateSession(_ context.Context, sess *store.Session) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, *sess)
	return nil
}

func (c *captureSessionCreator) AppendMessagesAutoSeq(_ context.Context, _ uuid.UUID, msgs []store.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, msgs...)
	return nil
}

func TestCreateAgentSession_TopLevelRunHasNilParent(t *testing.T) {
	creator := &captureSessionCreator{}
	a := &ExternalAgentActivities{Store: creator}

	sid := uuid.New()
	err := a.CreateAgentSession(t.Context(), CreateAgentSessionArgs{
		SessionID:  sid,
		WorkflowID: "wf-1",
		RunID:      "run-1",
	})
	require.NoError(t, err)
	require.Len(t, creator.calls, 1)

	got := creator.calls[0]
	assert.Equal(t, sid, got.ID)
	assert.Equal(t, "wf-1", got.WorkflowID)
	assert.Equal(t, "run-1", got.RunID)
	assert.Equal(t, store.SessionKindAgentTask, got.Kind)
	assert.Equal(t, store.SessionStatusActive, got.Status)
	assert.Nil(t, got.ParentSession, "top-level run must leave parent_session NULL")
}

func TestCreateAgentSession_FanoutItemPersisted(t *testing.T) {
	creator := &captureSessionCreator{}
	a := &ExternalAgentActivities{Store: creator}

	sid := uuid.New()
	parentID := uuid.New()
	item := json.RawMessage(`{"issue_number":42,"title":"fix bug"}`)

	err := a.CreateAgentSession(t.Context(), CreateAgentSessionArgs{
		SessionID:       sid,
		WorkflowID:      "wf-child-1",
		RunID:           "run-child-1",
		ParentSessionID: parentID,
		FanoutItem:      item,
	})
	require.NoError(t, err)

	require.Len(t, creator.calls, 1, "session row must be created")
	require.Len(t, creator.messages, 1, "fanout item must produce one message")

	msg := creator.messages[0]
	assert.Equal(t, sid, msg.SessionID)
	assert.Equal(t, ctxbuild.RoleContext, msg.Role)
	assert.Empty(t, msg.Content)

	var meta map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(msg.Metadata, &meta))
	assert.JSONEq(t, `"session_start"`, string(meta["event"]))
	assert.JSONEq(t, `{"issue_number":42,"title":"fix bug"}`, string(meta["fanout_item"]))
}

func TestCreateAgentSession_NoFanoutItemNoMessage(t *testing.T) {
	creator := &captureSessionCreator{}
	a := &ExternalAgentActivities{Store: creator}

	err := a.CreateAgentSession(t.Context(), CreateAgentSessionArgs{
		SessionID:  uuid.New(),
		WorkflowID: "wf-1",
		RunID:      "run-1",
	})
	require.NoError(t, err)
	assert.Len(t, creator.calls, 1)
	assert.Empty(t, creator.messages, "no fanout item means no extra message")
}

func TestCreateAgentSession_ChildRecordsParent(t *testing.T) {
	creator := &captureSessionCreator{}
	a := &ExternalAgentActivities{Store: creator}

	parentID := uuid.New()
	childID := uuid.New()
	err := a.CreateAgentSession(t.Context(), CreateAgentSessionArgs{
		SessionID:       childID,
		WorkflowID:      "wf-child",
		RunID:           "run-child",
		ParentSessionID: parentID,
	})
	require.NoError(t, err)
	require.Len(t, creator.calls, 1)

	got := creator.calls[0]
	require.NotNil(t, got.ParentSession, "fanout child must populate parent_session FK")
	assert.Equal(t, parentID, *got.ParentSession,
		"parent_session must match the supplied ParentSessionID")
	assert.Equal(t, childID, got.ID)
}

// --- fakeRunner and inMemoryEventSink for ExecAgentCLI tests ---

type fakeRunner struct {
	name   string
	emit   []runner.Event
	result runner.RunResult
	err    error
}

func (f *fakeRunner) Name() string { return f.name }
func (f *fakeRunner) Capabilities() runner.Capabilities {
	return runner.Capabilities{StructuredOutput: true}
}
func (f *fakeRunner) Run(_ context.Context, _ runner.RunConfig, events chan<- runner.Event) (runner.RunResult, error) { //nolint:gocritic // hugeParam: signature fixed by the runner.Runner interface
	for _, ev := range f.emit { //nolint:gocritic // rangeValCopy: test fake iterating a small fixed slice
		events <- ev
	}
	return f.result, f.err
}

type inMemoryEventSink struct {
	mu     sync.Mutex
	events []PersistedEvent
}

func (s *inMemoryEventSink) Persist(_ context.Context, sessionID uuid.UUID, phase string, ev runner.Event) error { //nolint:gocritic // hugeParam: signature fixed by the event sink interface
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, PersistedEvent{SessionID: sessionID, Phase: phase, Event: ev})
	return nil
}

func (s *inMemoryEventSink) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func (s *inMemoryEventSink) Broadcast(_ uuid.UUID, _ runner.Event) {} //nolint:gocritic // hugeParam: signature fixed by the event sink interface

func TestExecAgentCLI_PersistsEachEvent(t *testing.T) {
	// Override heartbeat to no-op for tests outside Temporal activity context.
	activityHeartbeat = func(_ context.Context, _ ...any) {}

	reg := runner.NewRegistry()
	reg.Register(&fakeRunner{
		name: "fake",
		emit: []runner.Event{
			{Kind: runner.KindSessionStart},
			{Kind: runner.KindTextDelta, Text: "hi"},
			{Kind: runner.KindDone, UsageDelta: runner.TokenUsage{TotalTokens: 42}},
		},
		result: runner.RunResult{ExitCode: 0, TotalUsage: runner.TokenUsage{TotalTokens: 42}},
	})

	sink := &inMemoryEventSink{}
	a := &ExternalAgentActivities{Runners: reg, EventSink: sink}

	out, err := a.ExecAgentCLI(t.Context(), ExecInput{
		SessionID:  uuid.New(),
		Runner:     "fake",
		Phase:      "main",
		ConfigName: "test-config",
		Config:     runner.RunConfig{Model: "x", Prompt: "hi", Steering: runner.SteeringScheduled},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, out.ExitCode)
	assert.Equal(t, 42, out.TotalUsage.TotalTokens)
	assert.Equal(t, 3, sink.Count(), "should persist 3 events")
}

func TestHydrateMCPSecrets_RestoresHeadersAndEnv(t *testing.T) {
	configs := []agentcfg.AgentConfig{{
		Name: "my-config",
		Phases: []agentcfg.Phase{{
			Name: "main",
			Tools: agentcfg.Tools{
				MCP: agentcfg.MCP{Servers: map[string]agentcfg.MCPServerConfig{
					"hindsight": {
						URL:     "http://localhost:8890/mcp/",
						Headers: map[string]string{"Authorization": "Bearer secret"},
						Env:     map[string]string{"API_KEY": "sk-123"},
					},
				}},
			},
		}},
	}}

	cfg := runner.RunConfig{
		MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
			"hindsight": {URL: "http://localhost:8890/mcp/"},
		}},
	}

	hydrateMCPSecrets(&cfg, configs, "my-config", "", "", "main")

	srv := cfg.MCP.Servers["hindsight"]
	assert.Equal(t, "Bearer secret", srv.Headers["Authorization"])
	assert.Equal(t, "sk-123", srv.Env["API_KEY"])

	// Verify memory isolation: mutating hydrated maps must not affect the original.
	srv.Headers["Authorization"] = "mutated"
	origHeaders := configs[0].Phases[0].Tools.MCP.Servers["hindsight"].Headers
	assert.Equal(t, "Bearer secret", origHeaders["Authorization"],
		"hydrated map must be a deep copy, not a shared reference")
}

func TestHydrateMCPSecrets_NoMatchingConfig(t *testing.T) {
	configs := []agentcfg.AgentConfig{{Name: "other"}}
	cfg := runner.RunConfig{
		MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
			"x": {URL: "http://x"},
		}},
	}

	hydrateMCPSecrets(&cfg, configs, "nonexistent", "", "", "main")

	assert.Nil(t, cfg.MCP.Servers["x"].Headers, "should remain nil when config not found")
}

func TestHydrateMCPSecrets_NoMCPServers(t *testing.T) {
	configs := []agentcfg.AgentConfig{{Name: "x"}}
	cfg := runner.RunConfig{}

	hydrateMCPSecrets(&cfg, configs, "x", "", "", "main")
	assert.Empty(t, cfg.MCP.Servers)
}

func TestHydrateMCPSecrets_PartialServerMatch(t *testing.T) {
	configs := []agentcfg.AgentConfig{{
		Name: "cfg",
		Phases: []agentcfg.Phase{{
			Name: "p",
			Tools: agentcfg.Tools{
				MCP: agentcfg.MCP{Servers: map[string]agentcfg.MCPServerConfig{
					"a": {
						URL:     "http://a",
						Headers: map[string]string{"X-Token": "secret-a"},
					},
				}},
			},
		}},
	}}

	cfg := runner.RunConfig{
		MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
			"a": {URL: "http://a"},
			"b": {URL: "http://b"},
		}},
	}

	hydrateMCPSecrets(&cfg, configs, "cfg", "", "", "p")

	assert.Equal(t, "secret-a", cfg.MCP.Servers["a"].Headers["X-Token"])
	assert.Nil(t, cfg.MCP.Servers["b"].Headers, "server b has no secrets to hydrate")
}

func TestHydrateMCPSecrets_SubAgent(t *testing.T) {
	// A cli sub-agent nested under a supervisor task's SubAgents["analysis"],
	// with an MCP server whose Headers/Env were stripped for Temporal history.
	configs := []agentcfg.AgentConfig{{
		Name: "triage",
		Supervisor: agentcfg.Supervisor{
			SubAgents: map[string]agentcfg.AgentConfig{
				"analysis": {
					Phases: []agentcfg.Phase{{
						Name: "investigate",
						Tools: agentcfg.Tools{MCP: agentcfg.MCP{Servers: map[string]agentcfg.MCPServerConfig{
							"sentry": {
								URL:     "https://mcp.example/sse",
								Headers: map[string]string{"Authorization": "Bearer sekret"},
								Env:     map[string]string{"SENTRY_TOKEN": "st-123"},
							},
						}}},
					}},
				},
			},
		},
	}}

	// The RunConfig the sub-agent's runner would use: server present, creds
	// stripped. The sub-agent's own ConfigName is empty; the identity is the
	// parent task name plus the kind.
	cfg := runner.RunConfig{MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
		"sentry": {URL: "https://mcp.example/sse"},
	}}}

	hydrateMCPSecrets(&cfg, configs, "", "triage", "analysis", "investigate")

	srv := cfg.MCP.Servers["sentry"]
	assert.Equal(t, "Bearer sekret", srv.Headers["Authorization"])
	assert.Equal(t, "st-123", srv.Env["SENTRY_TOKEN"])

	// Deep copy: mutating the hydrated map must not affect the stored config.
	srv.Headers["Authorization"] = "mutated"
	orig := configs[0].Supervisor.SubAgents["analysis"].Phases[0].Tools.MCP.Servers["sentry"].Headers
	assert.Equal(t, "Bearer sekret", orig["Authorization"], "hydrated map must be a deep copy")
}

func TestHydrateMCPSecrets_SubAgentUnknownKind(t *testing.T) {
	configs := []agentcfg.AgentConfig{{
		Name:       "triage",
		Supervisor: agentcfg.Supervisor{SubAgents: map[string]agentcfg.AgentConfig{"analysis": {}}},
	}}
	cfg := runner.RunConfig{MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
		"sentry": {URL: "https://mcp"},
	}}}

	hydrateMCPSecrets(&cfg, configs, "", "triage", "missing", "investigate")

	assert.Nil(t, cfg.MCP.Servers["sentry"].Headers, "an unknown sub-agent kind must not hydrate anything")
}

func TestHydrateMCPSecrets_FanoutChild(t *testing.T) {
	// A fanout child phase name is "<phase>-<index>"; hydration strips the
	// index and pulls creds from the fanout child's own MCP servers. This locks
	// the fanout path the resolve/hydrate refactor preserves.
	configs := []agentcfg.AgentConfig{{
		Name: "task",
		Phases: []agentcfg.Phase{{
			Name: "per_issue",
			Fanout: &agentcfg.FanoutConfig{Child: agentcfg.FanoutChildConfig{
				Tools: agentcfg.Tools{MCP: agentcfg.MCP{Servers: map[string]agentcfg.MCPServerConfig{
					"sentry": {URL: "https://mcp", Headers: map[string]string{"Authorization": "Bearer sekret"}},
				}}},
			}},
		}},
	}}
	cfg := runner.RunConfig{MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
		"sentry": {URL: "https://mcp"},
	}}}

	hydrateMCPSecrets(&cfg, configs, "task", "", "", "per_issue-0")

	assert.Equal(t, "Bearer sekret", cfg.MCP.Servers["sentry"].Headers["Authorization"])
}

func TestHydrateMCPSecrets_SubAgentFanoutChild(t *testing.T) {
	// A cli sub-agent whose own phase fans out: the child phase name is
	// "<phase>-<index>", resolved through the sub-agent config (parent+kind)
	// and then the fanout "-N" strip. Locks the composed path fanout.go
	// inheritance preserves.
	configs := []agentcfg.AgentConfig{{
		Name: "triage",
		Supervisor: agentcfg.Supervisor{SubAgents: map[string]agentcfg.AgentConfig{
			"analysis": {Phases: []agentcfg.Phase{{
				Name: "per_issue",
				Fanout: &agentcfg.FanoutConfig{Child: agentcfg.FanoutChildConfig{
					Tools: agentcfg.Tools{MCP: agentcfg.MCP{Servers: map[string]agentcfg.MCPServerConfig{
						"sentry": {URL: "https://mcp", Headers: map[string]string{"Authorization": "Bearer child-sekret"}},
					}}},
				}},
			}}},
		}},
	}}
	cfg := runner.RunConfig{MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
		"sentry": {URL: "https://mcp"},
	}}}

	hydrateMCPSecrets(&cfg, configs, "", "triage", "analysis", "per_issue-0")

	assert.Equal(t, "Bearer child-sekret", cfg.MCP.Servers["sentry"].Headers["Authorization"])
}

func TestHydrateMCPSecrets_SubAgentUnknownParent(t *testing.T) {
	configs := []agentcfg.AgentConfig{{
		Name:       "triage",
		Supervisor: agentcfg.Supervisor{SubAgents: map[string]agentcfg.AgentConfig{"analysis": {}}},
	}}
	cfg := runner.RunConfig{MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
		"sentry": {URL: "https://mcp"},
	}}}

	hydrateMCPSecrets(&cfg, configs, "", "no-such-parent", "analysis", "investigate")

	assert.Nil(t, cfg.MCP.Servers["sentry"].Headers, "an unknown parent task must not hydrate anything")
}

func TestHydrateMCPSecrets_SubAgentKindDoesNotMatchTopLevelName(t *testing.T) {
	// A top-level config and a sub-agent kind share the name "analysis" but hold
	// different secrets. Resolving a sub-agent must use the nested config, never
	// a top-level config of the same name: no cross-config secret injection.
	configs := []agentcfg.AgentConfig{
		{
			Name: "analysis", // top-level config sharing the kind's name
			Phases: []agentcfg.Phase{{
				Name: "investigate",
				Tools: agentcfg.Tools{MCP: agentcfg.MCP{Servers: map[string]agentcfg.MCPServerConfig{
					"sentry": {URL: "https://mcp", Headers: map[string]string{"Authorization": "Bearer TOPLEVEL"}},
				}}},
			}},
		},
		{
			Name: "triage",
			Supervisor: agentcfg.Supervisor{SubAgents: map[string]agentcfg.AgentConfig{
				"analysis": {Phases: []agentcfg.Phase{{
					Name: "investigate",
					Tools: agentcfg.Tools{MCP: agentcfg.MCP{Servers: map[string]agentcfg.MCPServerConfig{
						"sentry": {URL: "https://mcp", Headers: map[string]string{"Authorization": "Bearer NESTED"}},
					}}},
				}}},
			}},
		},
	}
	cfg := runner.RunConfig{MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
		"sentry": {URL: "https://mcp"},
	}}}

	hydrateMCPSecrets(&cfg, configs, "", "triage", "analysis", "investigate")

	assert.Equal(t, "Bearer NESTED", cfg.MCP.Servers["sentry"].Headers["Authorization"],
		"must hydrate from the parent's nested sub-agent, not a top-level config of the same name")
}

func TestHydrateMCPSecrets_ExplicitHyphenatedPhaseNotMishydrated(t *testing.T) {
	// An explicit phase "scan-2" coexists with a fanout phase "scan". Executing
	// "scan-2" must hydrate from its OWN servers, not scan's fanout child (the
	// exact-match-first guard against a same-prefix collision).
	configs := []agentcfg.AgentConfig{{
		Name: "task",
		Phases: []agentcfg.Phase{
			{
				Name: "scan",
				Fanout: &agentcfg.FanoutConfig{Child: agentcfg.FanoutChildConfig{
					Tools: agentcfg.Tools{MCP: agentcfg.MCP{Servers: map[string]agentcfg.MCPServerConfig{
						"sentry": {URL: "https://mcp", Headers: map[string]string{"Authorization": "Bearer FANOUT"}},
					}}},
				}},
			},
			{
				Name: "scan-2",
				Tools: agentcfg.Tools{MCP: agentcfg.MCP{Servers: map[string]agentcfg.MCPServerConfig{
					"sentry": {URL: "https://mcp", Headers: map[string]string{"Authorization": "Bearer OWN"}},
				}}},
			},
		},
	}}
	cfg := runner.RunConfig{MCP: runner.MCPPolicy{Servers: map[string]runner.MCPServerConfig{
		"sentry": {URL: "https://mcp"},
	}}}

	hydrateMCPSecrets(&cfg, configs, "task", "", "", "scan-2")

	assert.Equal(t, "Bearer OWN", cfg.MCP.Servers["sentry"].Headers["Authorization"],
		"an explicit hyphenated phase must use its own servers, not a same-prefix fanout phase's child")
}

// --- capableRunner and stubGuidance for guidance wiring tests ---

// capableRunner is a test runner whose Capabilities and Run behaviour are
// configurable per test case. It is distinct from fakeRunner so that the
// existing ExecAgentCLI tests remain unchanged.
type capableRunner struct {
	name string
	caps runner.Capabilities
	run  func(context.Context, runner.RunConfig, chan<- runner.Event) (runner.RunResult, error)
}

func (c *capableRunner) Name() string                      { return c.name }
func (c *capableRunner) Capabilities() runner.Capabilities { return c.caps }
func (c *capableRunner) Run(ctx context.Context, cfg runner.RunConfig, ch chan<- runner.Event) (runner.RunResult, error) { //nolint:gocritic // hugeParam: signature fixed by the runner.Runner interface
	return c.run(ctx, cfg, ch)
}

type stubGuidance struct {
	registered string
	ch         chan string
}

func (s *stubGuidance) RegisterRun(runID string) {
	s.registered = runID
}

func (s *stubGuidance) DeregisterRun(string) {}

func (s *stubGuidance) Receiver(runID string) (<-chan string, bool) {
	if s.registered != runID {
		return nil, false
	}
	return s.ch, true
}

func TestExecAgentCLI_WiresGuidanceChannelForBidiRunner(t *testing.T) {
	gotCfg := make(chan runner.RunConfig, 1)
	stub := &capableRunner{
		name: "claude",
		caps: runner.Capabilities{BidirectionalStream: true},
		run: func(_ context.Context, cfg runner.RunConfig, _ chan<- runner.Event) (runner.RunResult, error) {
			gotCfg <- cfg
			return runner.RunResult{}, nil
		},
	}
	reg := runner.NewRegistry()
	reg.Register(stub)

	guidance := &stubGuidance{ch: make(chan string, 4)}

	acts := &ExternalAgentActivities{
		Runners:  reg,
		Guidance: guidance,
	}

	_, err := acts.ExecAgentCLI(t.Context(), ExecInput{
		SessionID: uuid.New(),
		Runner:    "claude",
		Config:    runner.RunConfig{},
	})
	require.NoError(t, err)

	select {
	case cfg := <-gotCfg:
		require.NotNil(t, cfg.Guidance, "expected Guidance channel for bidi runner")
	case <-time.After(time.Second):
		t.Fatal("runner.Run was not invoked")
	}
}

func TestExecAgentCLI_NoGuidanceChannelForNonBidiRunner(t *testing.T) {
	gotCfg := make(chan runner.RunConfig, 1)
	stub := &capableRunner{
		name: "gemini",
		caps: runner.Capabilities{BidirectionalStream: false},
		run: func(_ context.Context, cfg runner.RunConfig, _ chan<- runner.Event) (runner.RunResult, error) {
			gotCfg <- cfg
			return runner.RunResult{}, nil
		},
	}
	reg := runner.NewRegistry()
	reg.Register(stub)

	guidance := &stubGuidance{ch: make(chan string, 4)}

	acts := &ExternalAgentActivities{
		Runners:  reg,
		Guidance: guidance,
	}

	_, err := acts.ExecAgentCLI(t.Context(), ExecInput{
		SessionID: uuid.New(),
		Runner:    "gemini",
		Config:    runner.RunConfig{},
	})
	require.NoError(t, err)

	select {
	case cfg := <-gotCfg:
		require.Nil(t, cfg.Guidance, "expected no Guidance channel for non-bidi runner")
	case <-time.After(time.Second):
		t.Fatal("runner.Run was not invoked")
	}
}

// --- State.Bank env injection tests ---

func TestPreparePhaseInput_StateBankSetsEnv(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.md"), []byte("go"), 0o644))

	a := &ExternalAgentActivities{}
	got, err := a.PreparePhaseInput(t.Context(), PreparePhaseInputArgs{
		SessionID:  uuid.New(),
		Phase:      agentcfg.Phase{Name: "main", Model: "claude-sonnet-4-6", Prompt: agentcfg.Prompt{Base: "base.md"}, Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled}},
		ConfigName: "x",
		Runner:     "claude",
		PromptDir:  dir,
		StateBank:  "alfred-tickets",
	})
	require.NoError(t, err)
	require.NotNil(t, got.Config.Env, "Env must be populated when StateBank is set")
	assert.Equal(t, "alfred-tickets", got.Config.Env["HINDSIGHT_BANK"])
}

func TestPreparePhaseInput_EmptyStateBankNoEnv(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.md"), []byte("go"), 0o644))

	a := &ExternalAgentActivities{}
	got, err := a.PreparePhaseInput(t.Context(), PreparePhaseInputArgs{
		SessionID:  uuid.New(),
		Phase:      agentcfg.Phase{Name: "main", Model: "claude-sonnet-4-6", Prompt: agentcfg.Prompt{Base: "base.md"}, Steering: agentcfg.Steering{Mode: agentcfg.SteeringScheduled}},
		ConfigName: "x",
		Runner:     "claude",
		PromptDir:  dir,
	})
	require.NoError(t, err)
	assert.Nil(t, got.Config.Env, "Env must be nil when StateBank is empty")
}

// --- RunStateHook tests ---

func TestRunStateHook_Success(t *testing.T) {
	a := &ExternalAgentActivities{}
	err := a.RunStateHook(t.Context(), agentcfg.StateHook{
		Type:    "bash",
		Command: "echo done",
	})
	assert.NoError(t, err)
}

func TestRunStateHook_FailureIsBestEffort(t *testing.T) {
	a := &ExternalAgentActivities{}
	err := a.RunStateHook(t.Context(), agentcfg.StateHook{
		Type:    "bash",
		Command: "exit 1",
	})
	assert.NoError(t, err, "state hooks are best-effort and must not return errors")
}

func TestRunStateHook_UnsupportedTypeIgnored(t *testing.T) {
	a := &ExternalAgentActivities{}
	err := a.RunStateHook(t.Context(), agentcfg.StateHook{
		Type:    "python",
		Command: "print('hi')",
	})
	assert.NoError(t, err, "unsupported hook types are ignored, not errors")
}

// --- dialAddress tests ---

func TestDialAddress(t *testing.T) {
	tests := []struct {
		name   string
		listen string
		want   string
	}{
		{"bare port", ":8086", "127.0.0.1:8086"},
		{"all-interfaces IPv4", "0.0.0.0:8086", "127.0.0.1:8086"},
		{"all-interfaces IPv6 bracketed maps to IPv6 loopback", "[::]:8086", "[::1]:8086"},
		{"explicit loopback unchanged", "127.0.0.1:8086", "127.0.0.1:8086"},
		{"explicit IPv6 loopback unchanged", "[::1]:8086", "[::1]:8086"},
		{"explicit non-wildcard IPv6 unchanged", "[2001:db8::1]:8086", "[2001:db8::1]:8086"},
		{"IPv6 zone ID unchanged", "[fe80::1%eth0]:8086", "[fe80::1%eth0]:8086"},
		{"explicit host preserved", "192.168.1.5:8086", "192.168.1.5:8086"},
		{"hostname preserved", "alfred-temporal.koti:8086", "alfred-temporal.koti:8086"},
		{"malformed (no port) returned unchanged", "not-a-host-port", "not-a-host-port"},
		{"port only, no colon, returned unchanged", "8086", "8086"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, dialAddress(tt.listen))
		})
	}
}

// TestExecAgentCLI_PermURLIsDialable is a unit-level guard on dialAddress's
// output shape: url.Parse never rejects a hostless authority like
// "http://:8086/..." and u.Host stays non-empty (it includes the port), so
// the faithful check for #118's actual symptom is u.Hostname(), not u.Host.
func TestExecAgentCLI_PermURLIsDialable(t *testing.T) {
	u, err := url.Parse("http://" + dialAddress(":8086") + "/mcp/perm/test-run")
	require.NoError(t, err)
	assert.NotEmpty(t, u.Hostname(), "dial URL must have a host, or no HTTP client can reach the MCP permission bridge")
}

// stubPermits is a minimal PermitRegistrar fake for exercising the real
// interactive-steering branch of ExecAgentCLI.
type stubPermits struct{}

func (stubPermits) RegisterRun(string, time.Duration) string { return "test-token" }
func (stubPermits) DeregisterRun(string)                     {}

// TestExecAgentCLI_InteractiveSteeringUsesDialableApprovalURL is the actual
// regression guard for #118: it drives ExecAgentCLI's real
// Steering==SteeringInteractive branch (internal/workflow/external_agent_activities.go
// around line 337) with a bare-port PermAddress, the documented config.yaml
// default form, and asserts the "alfred-perm" MCP server URL the runner
// actually receives has a real, dialable host. TestDialAddress and
// TestExecAgentCLI_PermURLIsDialable above only exercise dialAddress in
// isolation; neither would catch a regression at this call site itself
// (e.g. reverting to the raw a.PermAddress, or an argument-order slip).
func TestExecAgentCLI_InteractiveSteeringUsesDialableApprovalURL(t *testing.T) {
	gotCfg := make(chan runner.RunConfig, 1)
	stub := &capableRunner{
		name: "claude",
		run: func(_ context.Context, cfg runner.RunConfig, _ chan<- runner.Event) (runner.RunResult, error) {
			gotCfg <- cfg
			return runner.RunResult{}, nil
		},
	}
	reg := runner.NewRegistry()
	reg.Register(stub)

	acts := &ExternalAgentActivities{
		Runners:     reg,
		Permits:     stubPermits{},
		PermAddress: ":8086", // the documented config.yaml.example / deployed default form
	}

	_, err := acts.ExecAgentCLI(t.Context(), ExecInput{
		SessionID: uuid.New(),
		Runner:    "claude",
		Config:    runner.RunConfig{Steering: runner.SteeringInteractive},
	})
	require.NoError(t, err)

	select {
	case cfg := <-gotCfg:
		permServer, ok := cfg.MCP.Servers["alfred-perm"]
		require.True(t, ok, "expected alfred-perm MCP server to be injected for interactive steering")
		u, err := url.Parse(permServer.URL)
		require.NoError(t, err)
		assert.NotEmpty(t, u.Hostname(), "alfred-perm URL %q has no host; the claude subprocess can never reach the approval callback (#118)", permServer.URL)
		assert.Equal(t, "127.0.0.1", u.Hostname())
	case <-time.After(time.Second):
		t.Fatal("runner.Run was not invoked")
	}
}
