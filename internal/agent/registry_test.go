package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tphakala/alfred/internal/llm"
)

type fakeTool struct {
	name     string
	execErr  error
	execResp any
}

func (f *fakeTool) Name() string { return f.name }
func (f *fakeTool) Declaration() *llm.FunctionDeclaration {
	return &llm.FunctionDeclaration{Name: f.name}
}
func (f *fakeTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	return f.execResp, f.execErr
}
func (f *fakeTool) Idempotent() bool { return true }

func TestRegistry_LookupMissing(t *testing.T) {
	r := NewRegistry()
	_, ok := r.Lookup("nope")
	assert.False(t, ok)
}

func TestRegistry_ExecuteNotFoundReturnsErrToolNotFound(t *testing.T) {
	r := NewRegistry()
	_, err := r.Execute(t.Context(), "nope", nil)
	assert.ErrorIs(t, err, ErrToolNotFound)
}

func TestRegistry_ExecuteDelegatesToTool(t *testing.T) {
	want := map[string]any{"ok": true}
	r := NewRegistry(&fakeTool{name: "t", execResp: want})
	got, err := r.Execute(t.Context(), "t", nil)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestRegistry_DeclarationsReturnsAllTools(t *testing.T) {
	r := NewRegistry(&fakeTool{name: "b"}, &fakeTool{name: "a"}) // registered out of order
	decls := r.Declarations()
	require.Len(t, decls, 2)
	// Sorted alphabetically regardless of registration order.
	assert.Equal(t, "a", decls[0].Name)
	assert.Equal(t, "b", decls[1].Name)
}

func TestRegistry_DuplicateNamesPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate tool name")
		}
	}()
	NewRegistry(&fakeTool{name: "dup"}, &fakeTool{name: "dup"})
}

func TestRegistry_ExecutePropagatesToolError(t *testing.T) {
	boom := errors.New("tool boom")
	r := NewRegistry(&fakeTool{name: "t", execErr: boom})
	_, err := r.Execute(t.Context(), "t", nil)
	assert.ErrorIs(t, err, boom)
}
