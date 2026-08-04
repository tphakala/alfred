package server

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGuidanceStore_RegisterAndReceive(t *testing.T) {
	gs := NewGuidanceStore()
	ch, ok := gs.Receiver("run-1")
	assert.False(t, ok, "no channel before RegisterRun")
	assert.Nil(t, ch)

	gs.RegisterRun("run-1")
	defer gs.DeregisterRun("run-1")

	ch, ok = gs.Receiver("run-1")
	require.True(t, ok)
	require.NotNil(t, ch)

	pushed := gs.Push("run-1", "do the thing")
	assert.True(t, pushed, "Push should succeed for registered run")

	select {
	case got := <-ch:
		assert.Equal(t, "do the thing", got)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("guidance not received")
	}
}

func TestGuidanceStore_PushUnknownRun(t *testing.T) {
	gs := NewGuidanceStore()
	pushed := gs.Push("nonexistent", "ignored")
	assert.False(t, pushed)
}

func TestGuidanceStore_DeregisterClosesChannel(t *testing.T) {
	gs := NewGuidanceStore()
	gs.RegisterRun("run-1")

	ch, _ := gs.Receiver("run-1")

	gs.DeregisterRun("run-1")

	// Reading from a closed channel returns immediately with zero value
	// and ok == false.
	select {
	case v, ok := <-ch:
		assert.False(t, ok)
		assert.Equal(t, "", v)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel was not closed by DeregisterRun")
	}
}

func TestGuidanceStore_PushFullBufferDropsAndReports(t *testing.T) {
	gs := NewGuidanceStore()
	gs.RegisterRun("run-1")
	defer gs.DeregisterRun("run-1")

	// Fill the channel buffer (default 16). The 17th push must return false.
	for i := 0; i < guidanceChannelBuffer; i++ {
		require.True(t, gs.Push("run-1", "x"), "push %d should succeed", i)
	}
	assert.False(t, gs.Push("run-1", "overflow"))
}

// TestGuidanceStore_ConcurrentPushAndDeregister_NoPanic exercises the race
// between Push (lookup + send) and DeregisterRun (close + delete). Without
// holding the mutex through the select in Push, a goroutine could send on a
// channel already closed by DeregisterRun and panic. Run under -race to catch
// regressions.
func TestGuidanceStore_ConcurrentPushAndDeregister_NoPanic(t *testing.T) {
	gs := NewGuidanceStore()
	gs.RegisterRun("run-1")

	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				_ = gs.Push("run-1", "x")
			}
		}()
	}

	time.Sleep(20 * time.Millisecond)
	gs.DeregisterRun("run-1")
	close(done)
	wg.Wait()

	// Pushes after deregister all return false; no panic.
	assert.False(t, gs.Push("run-1", "post-deregister"))
}
