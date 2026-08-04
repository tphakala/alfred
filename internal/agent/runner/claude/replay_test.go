package claude

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/agent/runner"
)

func TestParseLine_SystemReplay_ReturnsKindReplay(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"replay","message":"stop and look at issue 42"}`)
	ev, ok := ParseLine(line)
	require.True(t, ok)
	assert.Equal(t, runner.KindReplay, ev.Kind)
	assert.Equal(t, "stop and look at issue 42", ev.Text)
	assert.JSONEq(t, string(line), string(ev.Raw))
}

func TestParseLine_SystemReplay_MissingMessage(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"replay"}`)
	ev, ok := ParseLine(line)
	require.True(t, ok)
	assert.Equal(t, runner.KindReplay, ev.Kind)
	assert.Equal(t, "", ev.Text)
}

func TestParseLine_SystemReplay_MalformedJSON(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"replay","message":`)
	ev, ok := ParseLine(line)
	assert.False(t, ok)
	assert.Equal(t, runner.Event{}, ev)
	_ = json.RawMessage(line) // keep import live
}
