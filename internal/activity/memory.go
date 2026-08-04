package activity

import (
	"context"
	"fmt"

	"github.com/tphakala/alfred/internal/memory"
)

// RetainInput holds parameters for the Retain activity.
type RetainInput struct {
	Content  string
	Tags     []string
	Metadata map[string]string
}

// MemoryActivities groups Hindsight memory-related Temporal activity implementations.
type MemoryActivities struct {
	client memory.MemoryClient
}

// NewMemoryActivities returns a new MemoryActivities backed by the given client.
func NewMemoryActivities(client memory.MemoryClient) *MemoryActivities {
	return &MemoryActivities{client: client}
}

// Recall queries Hindsight for memories similar to the given query string.
func (a *MemoryActivities) Recall(ctx context.Context, query string) ([]memory.MemoryItem, error) {
	resp, err := a.client.Recall(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("activity: recall: %w", err)
	}
	return resp.Memories, nil
}

// Retain stores a memory in Hindsight with the provided content, tags, and metadata.
func (a *MemoryActivities) Retain(ctx context.Context, input RetainInput) error {
	req := memory.RetainRequest{
		Content:  input.Content,
		Tags:     input.Tags,
		Metadata: input.Metadata,
	}
	if err := a.client.Retain(ctx, req); err != nil {
		return fmt.Errorf("activity: retain: %w", err)
	}
	return nil
}
