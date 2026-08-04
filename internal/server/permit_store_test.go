package server_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/server"
)

func TestPermitStore_RegisterAndValidateToken(t *testing.T) {
	ps := server.NewPermitStore()
	token := ps.RegisterRun("run-1", 10*time.Minute)
	assert.NotEmpty(t, token)
	assert.True(t, ps.ValidateToken("run-1", token))
	assert.False(t, ps.ValidateToken("run-1", "wrong-token"))
	assert.False(t, ps.ValidateToken("run-2", token))
}

func TestPermitStore_DeregisterRun(t *testing.T) {
	ps := server.NewPermitStore()
	token := ps.RegisterRun("run-1", 10*time.Minute)
	ps.DeregisterRun("run-1")
	assert.False(t, ps.ValidateToken("run-1", token))
}

func TestPermitStore_ApprovalChannel(t *testing.T) {
	ps := server.NewPermitStore()
	ps.RegisterRun("run-1", 10*time.Minute)
	ch := ps.CreateApprovalChannel("run-1", "call-1")
	require.NotNil(t, ch)

	go func() {
		ps.ResolveApproval("run-1", "call-1", server.ApprovalResolution{Decision: "allow"})
	}()

	select {
	case res := <-ch:
		assert.Equal(t, "allow", res.Decision)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for approval resolution")
	}
}

func TestPermitStore_ResolveApproval_NoChannel(t *testing.T) {
	ps := server.NewPermitStore()
	ok := ps.ResolveApproval("run-1", "call-1", server.ApprovalResolution{Decision: "allow"})
	assert.False(t, ok)
}

func TestPermitStore_PendingApprovals(t *testing.T) {
	ps := server.NewPermitStore()
	ps.RegisterRun("run-1", 10*time.Minute)
	require.NotNil(t, ps.CreateApprovalChannel("run-1", "call-1"))
	require.NotNil(t, ps.CreateApprovalChannel("run-1", "call-2"))

	pending := ps.PendingApprovals("run-1")
	assert.ElementsMatch(t, []string{"call-1", "call-2"}, pending)

	// Resolving a call removes it from the pending set.
	ch := ps.CreateApprovalChannel("run-1", "call-1")
	require.NotNil(t, ch)
	go func() {
		ps.ResolveApproval("run-1", "call-1", server.ApprovalResolution{Decision: "allow"})
	}()
	<-ch
	assert.ElementsMatch(t, []string{"call-2"}, ps.PendingApprovals("run-1"))

	// An unknown run yields no pending approvals.
	assert.Empty(t, ps.PendingApprovals("run-unknown"))
}

func TestPermitStore_SessionPolicy_DefaultEmpty(t *testing.T) {
	ps := server.NewPermitStore()
	ps.RegisterRun("run-1", 10*time.Minute)
	policy := ps.GetSessionPolicy("run-1")
	assert.False(t, policy.AutoApproveAll)
	assert.Empty(t, policy.AutoApproveTools)
}

func TestPermitStore_SessionPolicy_SetAndGet(t *testing.T) {
	ps := server.NewPermitStore()
	ps.RegisterRun("run-1", 10*time.Minute)
	ps.SetSessionPolicy("run-1", server.SessionApprovalPolicy{AutoApproveAll: true})
	assert.True(t, ps.GetSessionPolicy("run-1").AutoApproveAll)
}

func TestPermitStore_SessionPolicy_Extend(t *testing.T) {
	ps := server.NewPermitStore()
	ps.RegisterRun("run-1", 10*time.Minute)
	ps.ExtendSessionPolicy("run-1", server.SessionApprovalPolicyDelta{AddTools: []string{"Bash", "Read"}})
	policy := ps.GetSessionPolicy("run-1")
	assert.True(t, policy.AutoApproveTools["Bash"])
	assert.True(t, policy.AutoApproveTools["Read"])
}

func TestPermitStore_CheckPolicy_AutoApproveAll(t *testing.T) {
	ps := server.NewPermitStore()
	ps.RegisterRun("run-1", 10*time.Minute)
	ps.SetSessionPolicy("run-1", server.SessionApprovalPolicy{AutoApproveAll: true})
	assert.Equal(t, "allow", ps.CheckPolicy("run-1", "Bash", nil))
}

func TestPermitStore_CheckPolicy_AutoApproveTool(t *testing.T) {
	ps := server.NewPermitStore()
	ps.RegisterRun("run-1", 10*time.Minute)
	ps.ExtendSessionPolicy("run-1", server.SessionApprovalPolicyDelta{AddTools: []string{"Read"}})
	assert.Equal(t, "allow", ps.CheckPolicy("run-1", "Read", nil))
	assert.Equal(t, "", ps.CheckPolicy("run-1", "Bash", nil))
}

func TestPermitStore_CheckPolicy_DenyTool(t *testing.T) {
	ps := server.NewPermitStore()
	ps.RegisterRun("run-1", 10*time.Minute)
	ps.SetSessionPolicy("run-1", server.SessionApprovalPolicy{
		AutoApproveAll: true,
		DenyTools:      map[string]bool{"Bash": true},
	})
	assert.Equal(t, "deny", ps.CheckPolicy("run-1", "Bash", nil))
}
