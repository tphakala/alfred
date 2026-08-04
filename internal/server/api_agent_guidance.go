package server

import (
	"context"
	"errors"
)

// ErrGuidanceUnsupported is the server-level sentinel a WorkflowUpdater
// returns (wrapped via fmt.Errorf %w) when the workflow's inject_guidance
// handler refused the request because the runner lacks BidirectionalStream.
// It guards against the failure mode where the in-process RunnerCaps adapter
// is replaced with an asynchronous resolver that lets a non-bidi runner slip
// past the REST-level capability check; the typed signal lets the guidance
// handler still surface 409 instead of a misleading 500.
var ErrGuidanceUnsupported = errors.New("runner does not support mid-run guidance")

// GuidanceUpdateArgs mirrors workflow.GuidanceRequest. Duplicated here to keep
// the server package free of any direct dependency on the workflow package.
// The WorkflowUpdater implementation maps this to workflow.GuidanceRequest.
type GuidanceUpdateArgs struct {
	Text                          string
	RunnerCapsBidirectionalStream bool
}

// WorkflowUpdater dispatches Temporal updates from REST handlers.
type WorkflowUpdater interface {
	InjectGuidance(ctx context.Context, runID string, args GuidanceUpdateArgs) error
}

// RunnerCaps answers capability questions about a registered run's runner.
type RunnerCaps interface {
	BidirectionalStream(runnerName, runID string) bool
}
