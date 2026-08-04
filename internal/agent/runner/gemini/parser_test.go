package gemini

import (
	"bufio"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
)

func TestParseLine_AllKindsFromFixture(t *testing.T) {
	f, err := os.Open("testdata/sample_stream.jsonl")
	require.NoError(t, err)
	defer f.Close()

	var kinds []runner.EventKind
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		ev, ok := ParseLine(scanner.Bytes())
		if !ok {
			continue
		}
		kinds = append(kinds, ev.Kind)
	}
	require.NoError(t, scanner.Err())

	assert.Contains(t, kinds, runner.KindSessionStart)
	assert.Contains(t, kinds, runner.KindTextDelta)
	assert.Contains(t, kinds, runner.KindToolCall)
	assert.Contains(t, kinds, runner.KindToolResult)
	assert.Contains(t, kinds, runner.KindDone)
}

func TestParseLine_UnknownType(t *testing.T) {
	ev, ok := ParseLine([]byte(`{"type":"never_heard_of_it"}`))
	assert.False(t, ok)
	assert.Equal(t, runner.Event{}, ev)
}

func TestParseLine_MalformedJSON(t *testing.T) {
	ev, ok := ParseLine([]byte(`{"type":`))
	assert.False(t, ok)
	assert.Equal(t, runner.Event{}, ev)
}
