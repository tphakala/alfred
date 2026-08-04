package workflow

import (
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
)

// GuidanceRequest is the payload of the inject_guidance update.
// The caller is the REST endpoint at POST /api/agent-tasks/runs/{run_id}/guidance,
// which has already resolved the runner's BidirectionalStream capability and
// passes it through so the update handler can short-circuit cleanly.
// RunnerCapsBidirectionalStream is set by the REST handler (not from the HTTP
// request body) after checking the runner registry; it travels through Temporal
// update serialization so the workflow can gate on it without a registry call.
type GuidanceRequest struct {
	Text                          string `json:"text"`
	RunnerCapsBidirectionalStream bool   `json:"runner_caps_bidirectional_stream"`
}

// GuidanceUnsupportedError is returned by the inject_guidance update when the
// runner does not support mid-run guidance. The REST endpoint translates it
// into a 409 Conflict and the UI prompts the user with the cancel-and-restart
// fallback (Tier 1B).
type GuidanceUnsupportedError struct {
	Runner string
}

func (e *GuidanceUnsupportedError) Error() string {
	return fmt.Sprintf("runner %q does not support mid-run guidance", e.Runner)
}

// guidanceUnsupportedErrorTypeName is the string Temporal uses to identify
// GuidanceUnsupportedError once it has been wrapped into a NonRetryable
// ApplicationError on the wire. The Temporal SDK derives this string via
// reflection on the Go type the workflow returned (see
// go.temporal.io/sdk/internal getErrType); declared here as a const so it
// remains usable in switches and round-trip identity is asserted by a
// dedicated test that re-runs the reflection at test time.
const guidanceUnsupportedErrorTypeName = "GuidanceUnsupportedError"

// IsGuidanceUnsupportedError reports whether err is, wraps, or was
// transported over Temporal as a GuidanceUnsupportedError. In-process callers
// (test suites, workflow code) can use errors.As against the typed pointer;
// REST handlers that receive errors after Temporal's ApplicationError
// wrapping should call this helper so the typed signal survives the
// flattening into a string Type tag.
func IsGuidanceUnsupportedError(err error) bool {
	if _, ok := errors.AsType[*GuidanceUnsupportedError](err); ok {
		return true
	}
	if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok && appErr.Type() == guidanceUnsupportedErrorTypeName {
		return true
	}
	return false
}

// GuidanceLogEntry is appended to externalAgentState.GuidanceLog whenever an
// inject_guidance update is accepted. The log surfaces in the "state" query
// for operator visibility. ReceivedAt is the workflow.Now timestamp at accept
// time, so operators can correlate guidance with tool calls and turn events
// in the same run.
type GuidanceLogEntry struct {
	Text       string    `json:"text"`
	SequenceNo int       `json:"sequence_no"`
	ReceivedAt time.Time `json:"received_at"`
}
