package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTruncateToolResult_NilPassesThrough(t *testing.T) {
	assert.Nil(t, truncateToolResult(nil))
}

func TestTruncateToolResult_SmallPassesThrough(t *testing.T) {
	in := map[string]any{"a": "b"}
	out := truncateToolResult(in)
	assert.Equal(t, in, out)
}

func TestTruncateToolResult_LargeTruncated(t *testing.T) {
	big := strings.Repeat("x", 10_000)
	in := map[string]any{"text": big}
	out := truncateToolResult(in)
	m, ok := out.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, m["truncated"])
	originalBytes, ok := m["original_bytes"].(int)
	require.True(t, ok)
	assert.Greater(t, originalBytes, maxToolResultBytes)
	preview, ok := m["preview"].(string)
	require.True(t, ok)
	assert.Contains(t, preview, "xxx")
}
