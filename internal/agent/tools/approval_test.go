package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test constants for repeated string literals (goconst).
const (
	testActionAssignTicket = "assign ticket"
	testDescAssignTicket   = "Assign TICKET-123 to Alice"
	testDescAssignTicket2  = "Assign TICKET-123"

	testCallID       = "t1-c0"
	testCallIDKey    = "_callId"
	testEmitEventKey = "_emitEvent"
)

type fakeBroker struct {
	resp    ApprovalResponse
	err     error
	gotCall string
}

func (f *fakeBroker) WaitForApproval(_ context.Context, callID string, emitEvent func()) (ApprovalResponse, error) {
	f.gotCall = callID
	emitEvent()
	return f.resp, f.err
}

func TestApprovalTool_Approved(t *testing.T) {
	broker := &fakeBroker{resp: ApprovalResponse{Approved: true}}
	tool := NewApprovalTool(broker, nil)
	got, err := tool.Execute(t.Context(), map[string]any{
		argAction: testActionAssignTicket, argDescription: testDescAssignTicket,
		testCallIDKey: testCallID, testEmitEventKey: func() {},
	})
	require.NoError(t, err)
	m, ok := got.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, m["approved"])
	assert.Equal(t, testCallID, broker.gotCall)
}

func TestApprovalTool_Rejected(t *testing.T) {
	broker := &fakeBroker{resp: ApprovalResponse{Approved: false, Reason: "too risky"}}
	tool := NewApprovalTool(broker, nil)
	got, err := tool.Execute(t.Context(), map[string]any{
		argAction: "delete ticket", argDescription: "Delete TICKET-456",
		testCallIDKey: testCallID, testEmitEventKey: func() {},
	})
	require.NoError(t, err)
	m, ok := got.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, m["approved"])
	assert.Equal(t, "too risky", m["reason"])
}

func TestApprovalTool_MissingAction(t *testing.T) {
	tool := NewApprovalTool(&fakeBroker{}, nil)
	_, err := tool.Execute(t.Context(), map[string]any{
		argDescription: "do something", testCallIDKey: testCallID, testEmitEventKey: func() {},
	})
	assert.ErrorContains(t, err, "missing required argument")
}

func TestApprovalTool_MissingDescription(t *testing.T) {
	tool := NewApprovalTool(&fakeBroker{}, nil)
	_, err := tool.Execute(t.Context(), map[string]any{
		argAction: "do something", testCallIDKey: testCallID, testEmitEventKey: func() {},
	})
	assert.ErrorContains(t, err, "missing required argument")
}

func TestApprovalTool_BrokerError(t *testing.T) {
	broker := &fakeBroker{err: errors.New("timeout")}
	tool := NewApprovalTool(broker, nil)
	_, err := tool.Execute(t.Context(), map[string]any{
		argAction: testActionAssignTicket, argDescription: testDescAssignTicket2,
		testCallIDKey: testCallID, testEmitEventKey: func() {},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "timeout")
}

func TestApprovalTool_NilBrokerPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil broker")
		}
	}()
	NewApprovalTool(nil, nil)
}

func TestApprovalTool_NameAndDeclaration(t *testing.T) {
	tool := NewApprovalTool(&fakeBroker{}, nil)
	assert.Equal(t, toolNameRequestApproval, tool.Name())
	d := tool.Declaration()
	require.NotNil(t, d)
	assert.Equal(t, toolNameRequestApproval, d.Name)
	require.NotNil(t, d.Parameters)
	assert.Contains(t, d.Parameters.Properties, argAction)
	assert.Contains(t, d.Parameters.Properties, argDescription)
	assert.Contains(t, d.Parameters.Required, argAction)
	assert.Contains(t, d.Parameters.Required, argDescription)
}

func TestPanicBroker_Panics(t *testing.T) {
	broker := NewPanicBroker()
	assert.Panics(t, func() {
		_, _ = broker.WaitForApproval(t.Context(), "call-1", func() {})
	})
}

func TestPanicBroker_SatisfiesApprovalBroker(t *testing.T) {
	var _ ApprovalBroker = PanicBroker{}
	tool := NewApprovalTool(NewPanicBroker(), nil)
	assert.Equal(t, "request_approval", tool.Name())
}
