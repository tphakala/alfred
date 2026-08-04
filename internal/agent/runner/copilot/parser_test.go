package copilot

import (
	"bufio"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
)

func TestParseLine_KnownAndUnknownKinds(t *testing.T) {
	f, err := os.Open("testdata/sample_stream.jsonl")
	require.NoError(t, err)
	defer f.Close()

	kinds := []runner.EventKind{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		ev, ok := ParseLine(scanner.Bytes())
		require.True(t, ok, "every documented line should parse to some Event, even Unknown")
		kinds = append(kinds, ev.Kind)
	}
	require.NoError(t, scanner.Err())

	assert.Contains(t, kinds, runner.KindSessionStart)
	assert.Contains(t, kinds, runner.KindTextDelta)
	assert.Contains(t, kinds, runner.KindToolCall)
	assert.Contains(t, kinds, runner.KindToolResult)
	assert.Contains(t, kinds, runner.KindUnknown)
	assert.Contains(t, kinds, runner.KindDone)
}

func TestParseLine_MalformedJSON(t *testing.T) {
	ev, ok := ParseLine([]byte(`{not json`))
	assert.False(t, ok)
	assert.Equal(t, runner.Event{}, ev)
}
