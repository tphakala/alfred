package server

import (
	"context"
	"encoding/json"
)

// ApprovalNotifier sends approval state changes to the workflow's Temporal
// history. Implementations resolve the session UUID (runID) to the owning
// Temporal workflow and dispatch the appropriate signal or update. Errors
// are best-effort: callers log failures but do not let them break the
// in-process PermitStore approval flow.
type ApprovalNotifier interface {
	NotifyApprovalPending(ctx context.Context, runID, callID, toolName string, input json.RawMessage) error
	NotifyApprovalResolved(ctx context.Context, runID, callID, decision, reason string) error
}
