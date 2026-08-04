package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/server"
)

func TestMCPPerm_InitializeReturnsCapabilities(t *testing.T) {
	ps := server.NewPermitStore()
	handler := server.NewMCPPermHandler(ps, nil, nil)
	token := ps.RegisterRun("run-1", 10*time.Minute)

	body := jsonRPCRequest(1, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "claude", "version": "1.0"},
	})
	req := httptest.NewRequest("POST", "/mcp/perm/run-1", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", "run-1")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp jsonRPCResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Nil(t, resp.Error)
	result := resp.Result.(map[string]any)
	assert.Equal(t, "2025-03-26", result["protocolVersion"])
}

func TestMCPPerm_ToolsListReturnsPermissionRequest(t *testing.T) {
	ps := server.NewPermitStore()
	handler := server.NewMCPPermHandler(ps, nil, nil)
	token := ps.RegisterRun("run-1", 10*time.Minute)

	body := jsonRPCRequest(2, "tools/list", nil)
	req := httptest.NewRequest("POST", "/mcp/perm/run-1", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", "run-1")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp jsonRPCResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	tools := result["tools"].([]any)
	require.Len(t, tools, 1)
	tool := tools[0].(map[string]any)
	assert.Equal(t, "permission_request", tool["name"])
}

func TestMCPPerm_InvalidTokenReturns401(t *testing.T) {
	ps := server.NewPermitStore()
	handler := server.NewMCPPermHandler(ps, nil, nil)
	ps.RegisterRun("run-1", 10*time.Minute)

	body := jsonRPCRequest(1, "initialize", nil)
	req := httptest.NewRequest("POST", "/mcp/perm/run-1", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", "run-1")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMCPPerm_ToolCall_AutoApprove(t *testing.T) {
	ps := server.NewPermitStore()
	handler := server.NewMCPPermHandler(ps, nil, nil)
	token := ps.RegisterRun("run-1", 10*time.Minute)
	ps.SetSessionPolicy("run-1", server.SessionApprovalPolicy{AutoApproveAll: true})

	body := jsonRPCRequest(3, "tools/call", map[string]any{
		"name": "permission_request",
		"arguments": map[string]any{
			"tool_name": "Bash",
			"input":     map[string]any{"command": "ls"},
		},
	})
	req := httptest.NewRequest("POST", "/mcp/perm/run-1", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", "run-1")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp jsonRPCResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	content := extractToolResultText(t, resp.Result)
	var decision map[string]any
	require.NoError(t, json.Unmarshal([]byte(content), &decision))
	assert.Equal(t, "allow", decision["behavior"])
}

func TestMCPPerm_MethodNotAllowed(t *testing.T) {
	ps := server.NewPermitStore()
	handler := server.NewMCPPermHandler(ps, nil, nil)

	req := httptest.NewRequest("GET", "/mcp/perm/run-1", http.NoBody)
	req.SetPathValue("run_id", "run-1")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestMCPPerm_MissingRunID(t *testing.T) {
	ps := server.NewPermitStore()
	handler := server.NewMCPPermHandler(ps, nil, nil)

	req := httptest.NewRequest("POST", "/mcp/perm/", bytes.NewReader(jsonRPCRequest(1, "initialize", nil)))
	req.Header.Set("Content-Type", "application/json")
	// No SetPathValue for run_id

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestMCPPerm_UnknownMethod(t *testing.T) {
	ps := server.NewPermitStore()
	handler := server.NewMCPPermHandler(ps, nil, nil)
	token := ps.RegisterRun("run-1", 10*time.Minute)

	body := jsonRPCRequest(1, "unknown/method", nil)
	req := httptest.NewRequest("POST", "/mcp/perm/run-1", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", "run-1")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var resp jsonRPCResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotNil(t, resp.Error)
}

func TestMCPPerm_NotificationsInitialized(t *testing.T) {
	ps := server.NewPermitStore()
	handler := server.NewMCPPermHandler(ps, nil, nil)
	token := ps.RegisterRun("run-1", 10*time.Minute)

	body := jsonRPCRequest(nil, "notifications/initialized", nil)
	req := httptest.NewRequest("POST", "/mcp/perm/run-1", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", "run-1")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestMCPPerm_ToolCall_UnknownTool(t *testing.T) {
	ps := server.NewPermitStore()
	handler := server.NewMCPPermHandler(ps, nil, nil)
	token := ps.RegisterRun("run-1", 10*time.Minute)

	body := jsonRPCRequest(4, "tools/call", map[string]any{
		"name":      "not_a_tool",
		"arguments": map[string]any{},
	})
	req := httptest.NewRequest("POST", "/mcp/perm/run-1", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", "run-1")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var resp jsonRPCResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotNil(t, resp.Error)
}

func TestMCPPerm_ToolCall_ManualApproval(t *testing.T) {
	ps := server.NewPermitStore()
	handler := server.NewMCPPermHandler(ps, nil, nil)
	token := ps.RegisterRun("run-1", 10*time.Minute)
	// No auto-approve policy set; will block until resolved.

	body := jsonRPCRequest(5, "tools/call", map[string]any{
		"name": "permission_request",
		"arguments": map[string]any{
			"tool_name": "Bash",
			"input":     map[string]any{"command": "rm -rf /"},
		},
	})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest("POST", "/mcp/perm/run-1", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("run_id", "run-1")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		done <- w
	}()

	// Give the handler a moment to create the approval channel, then resolve.
	time.Sleep(50 * time.Millisecond)

	// Find the pending channel and resolve it. The call ID is tool_name + seq counter.
	// We'll resolve by iterating; since we only have one run/call, use ResolveApproval
	// with a known pattern. The handler uses "toolName-<seq>" as callID.
	// We need to find the right callID. Let's check all pending channels.
	resolved := false
	for i := int64(1); i <= 100; i++ {
		callID := fmt.Sprintf("Bash-%d", i)
		if ps.ResolveApproval("run-1", callID, server.ApprovalResolution{
			Decision: "deny",
			Reason:   "dangerous command",
		}) {
			resolved = true
			break
		}
	}
	require.True(t, resolved, "should have found and resolved the pending approval")

	select {
	case w := <-done:
		assert.Equal(t, http.StatusOK, w.Code)
		var resp jsonRPCResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		content := extractToolResultText(t, resp.Result)
		var decision map[string]any
		require.NoError(t, json.Unmarshal([]byte(content), &decision))
		assert.Equal(t, "deny", decision["behavior"])
		assert.Equal(t, "dangerous command", decision["message"])
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for handler response")
	}
}

func TestMCPPerm_ToolCall_NotifiesApprovalPending(t *testing.T) {
	ps := server.NewPermitStore()
	notifier := &stubApprovalNotifier{}
	handler := server.NewMCPPermHandler(ps, notifier, nil)
	token := ps.RegisterRun("run-1", 10*time.Minute)

	body := jsonRPCRequest(6, "tools/call", map[string]any{
		"name": "permission_request",
		"arguments": map[string]any{
			"tool_name": "Write",
			"input":     map[string]any{"path": "/tmp/out"},
		},
	})

	done := make(chan struct{}, 1)
	go func() {
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/mcp/perm/run-1", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("run_id", "run-1")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		done <- struct{}{}
	}()

	time.Sleep(50 * time.Millisecond)

	// Verify the notifier was called with correct args.
	snap := notifier.snapshot()
	require.Equal(t, 1, snap.pendingCount, "expected one pending notification")
	assert.Equal(t, "run-1", snap.lastPendingRunID)
	assert.Equal(t, "Write", snap.lastPendingToolName)
	assert.NotEmpty(t, snap.lastPendingCallID)

	// Resolve to unblock the handler.
	ps.ResolveApproval("run-1", snap.lastPendingCallID, server.ApprovalResolution{
		Decision: "allow",
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestMCPPerm_ToolCall_AutoApprove_NoNotification(t *testing.T) {
	ps := server.NewPermitStore()
	notifier := &stubApprovalNotifier{}
	handler := server.NewMCPPermHandler(ps, notifier, nil)
	token := ps.RegisterRun("run-1", 10*time.Minute)
	ps.SetSessionPolicy("run-1", server.SessionApprovalPolicy{AutoApproveAll: true})

	body := jsonRPCRequest(7, "tools/call", map[string]any{
		"name": "permission_request",
		"arguments": map[string]any{
			"tool_name": "Bash",
			"input":     map[string]any{"command": "ls"},
		},
	})
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/mcp/perm/run-1", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", "run-1")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	snap := notifier.snapshot()
	assert.Equal(t, 0, snap.pendingCount, "auto-approved calls should not notify")
}

// Test helpers

func jsonRPCRequest(id any, method string, params any) []byte {
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	return b
}

type jsonRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

func extractToolResultText(t *testing.T, result any) string {
	t.Helper()
	m := result.(map[string]any)
	content := m["content"].([]any)
	first := content[0].(map[string]any)
	return first["text"].(string)
}

type stubApprovalNotifier struct {
	mu                   sync.Mutex
	pendingCount         int
	lastPendingRunID     string
	lastPendingCallID    string
	lastPendingToolName  string
	resolvedCount        int
	lastResolvedRunID    string
	lastResolvedCallID   string
	lastResolvedDecision string
}

func (s *stubApprovalNotifier) NotifyApprovalPending(_ context.Context, runID, callID, toolName string, _ json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingCount++
	s.lastPendingRunID = runID
	s.lastPendingCallID = callID
	s.lastPendingToolName = toolName
	return nil
}

func (s *stubApprovalNotifier) NotifyApprovalResolved(_ context.Context, runID, callID, decision, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolvedCount++
	s.lastResolvedRunID = runID
	s.lastResolvedCallID = callID
	s.lastResolvedDecision = decision
	return nil
}

func (s *stubApprovalNotifier) snapshot() stubApprovalNotifier {
	s.mu.Lock()
	defer s.mu.Unlock()
	return stubApprovalNotifier{
		pendingCount:         s.pendingCount,
		lastPendingRunID:     s.lastPendingRunID,
		lastPendingCallID:    s.lastPendingCallID,
		lastPendingToolName:  s.lastPendingToolName,
		resolvedCount:        s.resolvedCount,
		lastResolvedRunID:    s.lastResolvedRunID,
		lastResolvedCallID:   s.lastResolvedCallID,
		lastResolvedDecision: s.lastResolvedDecision,
	}
}
