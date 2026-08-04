package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventKindString(t *testing.T) {
	assert.Equal(t, "text_delta", KindTextDelta.String())
	assert.Equal(t, "tool_call", KindToolCall.String())
	assert.Equal(t, "tool_result", KindToolResult.String())
	assert.Equal(t, "done", KindDone.String())
}

func TestCapabilitiesZeroValueIsAllFalse(t *testing.T) {
	var c Capabilities
	assert.False(t, c.StructuredOutput)
	assert.False(t, c.PerCallPermissionHook)
	assert.False(t, c.BidirectionalStream)
}

func TestEventJSONRoundTrip(t *testing.T) {
	e := Event{
		Kind:   KindToolCall,
		Round:  3,
		CallID: "tool_abc",
		Tool:   "Bash",
		Args:   json.RawMessage(`{"command":"ls"}`),
	}
	bs, err := json.Marshal(e)
	assert.NoError(t, err)
	var back Event
	assert.NoError(t, json.Unmarshal(bs, &back))
	assert.Equal(t, e.Kind, back.Kind)
	assert.Equal(t, e.CallID, back.CallID)
	assert.Equal(t, e.Tool, back.Tool)
}

type stubRunner struct{}

func (stubRunner) Name() string               { return "stub" }
func (stubRunner) Capabilities() Capabilities { return Capabilities{StructuredOutput: true} }
func (stubRunner) Run(_ context.Context, _ RunConfig, events chan<- Event) (RunResult, error) {
	events <- Event{Kind: KindDone}
	return RunResult{ExitCode: 0}, nil
}

func TestStubRunnerImplementsRunner(t *testing.T) {
	var _ Runner = stubRunner{}
}

func TestEventKind_Replay_RoundTrip(t *testing.T) {
	b, err := json.Marshal(KindReplay)
	require.NoError(t, err)
	assert.Equal(t, `"replay"`, string(b))

	var k EventKind
	require.NoError(t, json.Unmarshal([]byte(`"replay"`), &k))
	assert.Equal(t, KindReplay, k)
	assert.Equal(t, "replay", k.String())
}

func TestRunConfig_GuidanceField_Type(t *testing.T) {
	// Compile-time check that Guidance has the expected receive-only type.
	var cfg RunConfig
	var _ <-chan string = cfg.Guidance
}
