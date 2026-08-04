package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistry_GetReturnsRegistered(t *testing.T) {
	reg := NewRegistry()
	reg.Register(stubRunner{})

	got, ok := reg.Get("stub")
	require.True(t, ok)
	assert.Equal(t, "stub", got.Name())
}

func TestRegistry_GetMissingReturnsFalse(t *testing.T) {
	reg := NewRegistry()
	_, ok := reg.Get("missing")
	assert.False(t, ok)
}

func TestRegistry_DuplicateRegistrationPanics(t *testing.T) {
	reg := NewRegistry()
	reg.Register(stubRunner{})
	assert.Panics(t, func() { reg.Register(stubRunner{}) })
}

func TestRegistry_Names(t *testing.T) {
	reg := NewRegistry()
	reg.Register(stubRunner{})
	names := reg.Names()
	assert.Equal(t, []string{"stub"}, names)
}

func TestStubRunnerEmitsDone(t *testing.T) {
	events := make(chan Event, 1)
	_, err := stubRunner{}.Run(context.Background(), RunConfig{}, events)
	assert.NoError(t, err)
	assert.Equal(t, KindDone, (<-events).Kind)
}
