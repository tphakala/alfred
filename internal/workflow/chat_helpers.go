package workflow

import (
	"encoding/json"
	"fmt"

	"github.com/tphakala/alfred/internal/ctxbuild"
)

// safeString extracts a string from map[string]any without panicking on
// missing keys or wrong types.
func safeString(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// estimateTokens returns a rough token count for a string.
func estimateTokens(text string) int {
	return ctxbuild.EstimateTokens(text)
}

// streamTokenCount returns total tokens from an LLMStreamResult.
func streamTokenCount(result LLMStreamResult) int {
	if result.Usage == nil {
		return estimateTokens(result.Text)
	}
	return int(result.Usage.TotalTokens)
}

// toolTokenCount sums token estimates from tool messages.
func toolTokenCount(msgs []ActivityMessage) int {
	total := 0
	for _, m := range msgs {
		total += m.TokenEstimate
	}
	return total
}

// buildRoundMessages constructs the full set of messages for a round:
// model text (if any) followed by tool call/result pairs.
func buildRoundMessages(text string, toolMsgs []ActivityMessage) []ActivityMessage {
	var msgs []ActivityMessage
	if text != "" {
		msgs = append(msgs, ActivityMessage{
			Role:          ctxbuild.RoleModel,
			Content:       text,
			TokenEstimate: estimateTokens(text),
		})
	}
	msgs = append(msgs, toolMsgs...)
	return msgs
}

// buildToolMessages creates tool_call + tool_result ActivityMessage pairs.
func buildToolMessages(call LLMToolCall, result *ExecToolResult) []ActivityMessage {
	return []ActivityMessage{
		{
			Role:    ctxbuild.RoleToolCall,
			Content: call.Name,
			Metadata: map[string]any{
				metaKeyCallID: call.CallID,
				metaKeyArgs:   call.Args,
			},
		},
		{
			Role:          ctxbuild.RoleToolResult,
			Content:       result.Result,
			TokenEstimate: estimateTokens(result.Result),
			Metadata: map[string]any{
				metaKeyCallID: call.CallID,
				metaKeyName:   result.Name,
			},
		},
	}
}

// marshalApprovalPayload converts an ApprovalState to a JSON string for persistence.
func marshalApprovalPayload(approval *ApprovalState) string {
	payload := map[string]any{"approved": approval.Status == ApprovalStatusApproved}
	if approval.Reason != "" {
		payload["reason"] = approval.Reason
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf(`{"approved":%t}`, approval.Status == ApprovalStatusApproved)
	}
	return string(b)
}

// countPending returns the number of pending approvals in the map.
func countPending(approvals map[string]*ApprovalState) int {
	n := 0
	for _, a := range approvals {
		if a.Status == ApprovalStatusPending {
			n++
		}
	}
	return n
}
