package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
)

// MCPPermHandler handles MCP JSON-RPC 2.0 requests for the permission_request
// tool. Claude CLI calls this endpoint via --permission-prompt-tool to get
// approval for each tool call.
type MCPPermHandler struct {
	permits  *PermitStore
	notifier ApprovalNotifier
	logger   *slog.Logger
	seqID    atomic.Int64
}

// NewMCPPermHandler creates a new MCPPermHandler. The notifier is optional;
// when non-nil, pending approvals are signalled to Temporal for history
// recording. Errors from the notifier are logged but do not affect the
// in-process approval flow.
func NewMCPPermHandler(permits *PermitStore, notifier ApprovalNotifier, logger *slog.Logger) *MCPPermHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &MCPPermHandler{permits: permits, notifier: notifier, logger: logger}
}

// ServeHTTP handles incoming MCP JSON-RPC 2.0 requests. The run_id path
// parameter identifies which agent run this request belongs to, and the
// Bearer token in the Authorization header must match the registered token.
func (h *MCPPermHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	runID := r.PathValue("run_id")
	if runID == "" {
		http.Error(w, "missing run_id", http.StatusBadRequest)
		return
	}

	token := extractBearerToken(r)
	if !h.permits.ValidateToken(runID, token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}

	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPCError(w, nil, -32700, "parse error")
		return
	}

	switch req.Method {
	case "initialize":
		h.handleInitialize(w, req.ID)
	case "tools/list":
		h.handleToolsList(w, req.ID)
	case "tools/call":
		h.handleToolsCall(w, r, req.ID, req.Params, runID)
	case "notifications/initialized":
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSONRPCError(w, req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (h *MCPPermHandler) handleInitialize(w http.ResponseWriter, id any) {
	writeJSONRPCResult(w, id, map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "alfred-perm", "version": "1.0.0"},
	})
}

func (h *MCPPermHandler) handleToolsList(w http.ResponseWriter, id any) {
	writeJSONRPCResult(w, id, map[string]any{
		"tools": []map[string]any{
			{
				"name":        "permission_request",
				"description": "Request permission to execute a tool call",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"tool_name": map[string]any{"type": "string"},
						"input":     map[string]any{"type": "object"},
					},
					"required": []string{"tool_name", "input"},
				},
			},
		},
	})
}

func (h *MCPPermHandler) handleToolsCall(w http.ResponseWriter, r *http.Request, id any, params json.RawMessage, runID string) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		writeJSONRPCError(w, id, -32602, "invalid params")
		return
	}
	if call.Name != "permission_request" {
		writeJSONRPCError(w, id, -32602, fmt.Sprintf("unknown tool: %s", call.Name))
		return
	}

	var permReq struct {
		ToolName string          `json:"tool_name"`
		Input    json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(call.Arguments, &permReq); err != nil {
		writeJSONRPCError(w, id, -32602, "invalid permission_request arguments")
		return
	}

	decision := h.permits.CheckPolicy(runID, permReq.ToolName, permReq.Input)
	if decision != "" {
		h.logger.Info("auto-resolved permission", "run_id", runID, "tool", permReq.ToolName, "decision", decision)
		writeToolResult(w, id, decision, "auto-approved by session policy")
		return
	}

	callID := fmt.Sprintf("%s-%d", permReq.ToolName, h.seqID.Add(1))
	ch := h.permits.CreateApprovalChannel(runID, callID)
	if ch == nil {
		writeJSONRPCError(w, id, -32603, "run not found")
		return
	}

	h.logger.Info("permission request pending", "run_id", runID, "call_id", callID, "tool", permReq.ToolName)

	if h.notifier != nil {
		if err := h.notifier.NotifyApprovalPending(r.Context(), runID, callID, permReq.ToolName, permReq.Input); err != nil {
			h.logger.Warn("approval_pending signal failed", "run_id", runID, "call_id", callID, "tool", permReq.ToolName, "err", err)
		}
	}

	select {
	case res := <-ch:
		writeToolResult(w, id, res.Decision, res.Reason)
	case <-r.Context().Done():
		h.permits.RemoveApprovalChannel(runID, callID)
		writeJSONRPCError(w, id, -32603, "request cancelled")
	}
}

func writeToolResult(w http.ResponseWriter, id any, behavior, message string) {
	result := map[string]any{"behavior": behavior}
	if message != "" {
		result["message"] = message
	}
	resultJSON, _ := json.Marshal(result)
	writeJSONRPCResult(w, id, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(resultJSON)},
		},
	})
}

func writeJSONRPCResult(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func writeJSONRPCError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	if code == -32700 || code == -32602 {
		w.WriteHeader(http.StatusBadRequest)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return auth[7:]
}
