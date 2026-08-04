package workflow

import "encoding/json"

// PermissionRequest is the payload sent via Temporal Signal when the MCP
// permission bridge receives a tool permission check from Claude.
type PermissionRequest struct {
	RunID    string          `json:"run_id"`
	CallID   string          `json:"call_id"`
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input,omitempty"`
}

// PermissionResolution is the decision sent via the resolve_approval update.
type PermissionResolution struct {
	CallID   string `json:"call_id"`
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}
