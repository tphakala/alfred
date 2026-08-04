package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
)

func TestParser_AllSampleLines(t *testing.T) {
	f, err := os.Open("testdata/stream-sample.ndjson")
	require.NoError(t, err)
	defer f.Close()

	var events []runner.Event
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		ev, ok := ParseLine(scanner.Bytes())
		if !ok {
			continue
		}
		events = append(events, ev)
	}
	require.NoError(t, scanner.Err())

	require.Len(t, events, 7)
	assert.Equal(t, runner.KindSessionStart, events[0].Kind)
	assert.Equal(t, runner.KindTextDelta, events[1].Kind)
	assert.Equal(t, "Hello", events[1].Text)
	assert.Equal(t, runner.KindTextDelta, events[2].Kind)
	assert.Equal(t, " world", events[2].Text)
	assert.Equal(t, runner.KindToolCall, events[3].Kind)
	assert.Equal(t, "tool-abc", events[3].CallID)
	assert.Equal(t, "Bash", events[3].Tool)
	assert.JSONEq(t, `{"command":"ls"}`, string(events[3].Args))
	assert.Equal(t, runner.KindToolResult, events[4].Kind)
	assert.Equal(t, "tool-abc", events[4].CallID)
	assert.Equal(t, runner.KindApiRetry, events[5].Kind)
	assert.Contains(t, events[5].Text, "overloaded")
	assert.Equal(t, runner.KindDone, events[6].Kind)
	assert.Equal(t, 155, events[6].UsageDelta.TotalTokens)
}

func TestParser_UnknownTypeReturnsFalse(t *testing.T) {
	_, ok := ParseLine([]byte(`{"type":"future-event","data":42}`))
	assert.False(t, ok)
}

func TestParser_MalformedLineReturnsFalse(t *testing.T) {
	_, ok := ParseLine([]byte(`not json`))
	assert.False(t, ok)

	_, ok = ParseLine([]byte(`{"type":"system"`))
	assert.False(t, ok)
}

func TestParser_PreservesRaw(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init"}`)
	ev, ok := ParseLine(line)
	require.True(t, ok)
	assert.JSONEq(t, string(line), string(ev.Raw))
}

func TestParser_DoneCarriesCostAndUsage(t *testing.T) {
	line := []byte(`{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15},"total_cost_usd":0.001}`)
	ev, ok := ParseLine(line)
	require.True(t, ok)
	assert.Equal(t, runner.KindDone, ev.Kind)

	var done doneEnvelope
	require.NoError(t, json.Unmarshal(ev.Raw, &done))
	assert.Equal(t, 0.001, done.TotalCostUSD)
}
