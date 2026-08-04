package agentcfg

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSchemaProperties(t *testing.T) {
	t.Parallel()

	t.Run("returns the properties map", func(t *testing.T) {
		t.Parallel()
		raw := map[string]any{
			"query": map[string]any{"type": "string"},
			"limit": map[string]any{"type": "integer"},
		}
		got := SchemaProperties(raw)
		assert.Len(t, got, 2)
		assert.Contains(t, got, "query")
		assert.Contains(t, got, "limit")
	})

	t.Run("nil for a non-map", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, SchemaProperties("not a map"))
		assert.Nil(t, SchemaProperties([]any{"a"}))
		assert.Nil(t, SchemaProperties(nil))
	})

	t.Run("empty map stays a non-nil empty map", func(t *testing.T) {
		t.Parallel()
		got := SchemaProperties(map[string]any{})
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})
}

func TestSchemaStringList(t *testing.T) {
	t.Parallel()

	t.Run("returns the string elements", func(t *testing.T) {
		t.Parallel()
		got := SchemaStringList([]any{"query", "limit"})
		assert.Equal(t, []string{"query", "limit"}, got)
	})

	t.Run("skips non-string elements", func(t *testing.T) {
		t.Parallel()
		got := SchemaStringList([]any{"query", 42, true, "limit"})
		assert.Equal(t, []string{"query", "limit"}, got)
	})

	t.Run("nil for a non-list", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, SchemaStringList("query"))
		assert.Nil(t, SchemaStringList(map[string]any{"a": 1}))
		assert.Nil(t, SchemaStringList(nil))
	})

	t.Run("nil for an all-non-string list", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, SchemaStringList([]any{1, 2, 3}))
	})
}
