package claude

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamGuidance_WritesUserMessageJSON(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var buf safeBuffer
	guidance := make(chan string, 4)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		streamGuidance(ctx, &buf, guidance)
	}()

	guidance <- "stop and look at issue 42"
	guidance <- "also check the Sentry dashboard"

	// Allow the writer goroutine to drain.
	require.Eventually(t, func() bool {
		return strings.Count(buf.String(), "\n") >= 2
	}, time.Second, 10*time.Millisecond)

	close(guidance)
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], `"type":"user"`)
	assert.Contains(t, lines[0], `"text":"stop and look at issue 42"`)
	assert.Contains(t, lines[1], `"text":"also check the Sentry dashboard"`)
}

func TestStreamGuidance_ContextCancelExits(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	guidance := make(chan string, 1)
	var buf safeBuffer

	done := make(chan struct{})
	go func() {
		streamGuidance(ctx, &buf, guidance)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("streamGuidance did not exit on context cancel")
	}
}

// safeBuffer is a goroutine-safe bytes.Buffer for the test.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
