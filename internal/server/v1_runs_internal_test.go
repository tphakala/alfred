package server

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tphakala/alfred/internal/server/apiv1gen"
	"github.com/tphakala/alfred/internal/store"
)

func TestStrategyForKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind string
		want apiv1gen.RunStrategy
	}{
		{store.SessionKindChat, apiv1gen.RunStrategyReact},
		{store.SessionKindTicket, apiv1gen.RunStrategyStepDag},
		{store.SessionKindAgentTask, apiv1gen.RunStrategyFanout},
		{store.SessionKindMonitor, apiv1gen.RunStrategyMonitor},
		{"unknown-kind", apiv1gen.RunStrategyReact},
	}
	for _, tc := range cases {
		if got := strategyForKind(tc.kind); got != tc.want {
			t.Errorf("strategyForKind(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestSupportedActionsForKind(t *testing.T) {
	t.Parallel()

	chat := supportedActionsForKind(store.SessionKindChat)
	if !contains(chat, "messages") || !contains(chat, "approvals") {
		t.Errorf("chat actions %v missing messages or approvals", chat)
	}

	agent := supportedActionsForKind(store.SessionKindAgentTask)
	if !contains(agent, "children") || !contains(agent, "guidance") {
		t.Errorf("agent_task actions %v missing children or guidance", agent)
	}
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func TestMessageFromStore(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.June, 6, 12, 0, 0, 0, time.UTC)
	id := uuid.New()
	m := store.Message{
		ID:        id,
		Role:      "user",
		Content:   "hi",
		Sequence:  3,
		CreatedAt: now,
	}

	got := messageFromStore(&m)

	if got.Id != id.String() {
		t.Errorf("Id = %q, want %q", got.Id, id.String())
	}
	if got.Role != "user" {
		t.Errorf("Role = %q, want user", got.Role)
	}
	if got.Content == nil || *got.Content != "hi" {
		t.Errorf("Content = %v, want pointer to hi", got.Content)
	}
	if got.Sequence == nil || *got.Sequence != 3 {
		t.Errorf("Sequence = %v, want pointer to 3", got.Sequence)
	}
	if got.CreateTime == nil || !got.CreateTime.Equal(now) {
		t.Errorf("CreateTime = %v, want %v", got.CreateTime, now)
	}
}

func TestPolicyAPIRoundTrip(t *testing.T) {
	t.Parallel()

	in := apiv1gen.ApprovalPolicy{
		AutoApproveAll:   ptr(true),
		AutoApproveTools: ptr(map[string]bool{"Bash": true}),
		DenyTools:        ptr(map[string]bool{"Write": true}),
	}

	got := policyToAPI(policyFromAPI(in))

	if got.AutoApproveAll == nil || !*got.AutoApproveAll {
		t.Errorf("AutoApproveAll = %v, want pointer to true", got.AutoApproveAll)
	}
	if got.AutoApproveTools == nil || !(*got.AutoApproveTools)["Bash"] {
		t.Errorf("AutoApproveTools = %v, want Bash=true", got.AutoApproveTools)
	}
	if got.DenyTools == nil || !(*got.DenyTools)["Write"] {
		t.Errorf("DenyTools = %v, want Write=true", got.DenyTools)
	}
}

func TestRunFromSession(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.June, 6, 12, 0, 0, 0, time.UTC)
	parentID := uuid.New()
	sess := store.Session{
		ID:            uuid.New(),
		Kind:          store.SessionKindChat,
		Status:        store.SessionStatusActive,
		Epoch:         2,
		CreatedAt:     now,
		UpdatedAt:     now,
		ParentSession: &parentID,
	}

	run := runFromSession(sess)

	if run.Id != sess.ID {
		t.Errorf("Id = %v, want %v", run.Id, sess.ID)
	}
	if run.Kind != store.SessionKindChat {
		t.Errorf("Kind = %q, want %q", run.Kind, store.SessionKindChat)
	}
	if run.Status != store.SessionStatusActive {
		t.Errorf("Status = %q, want %q", run.Status, store.SessionStatusActive)
	}
	if run.Strategy != apiv1gen.RunStrategyReact {
		t.Errorf("Strategy = %q, want %q", run.Strategy, apiv1gen.RunStrategyReact)
	}
	if run.Epoch == nil || *run.Epoch != 2 {
		t.Errorf("Epoch = %v, want 2", run.Epoch)
	}
	if run.CreateTime == nil || !run.CreateTime.Equal(now) {
		t.Errorf("CreateTime = %v, want %v", run.CreateTime, now)
	}
	if run.UpdateTime == nil || !run.UpdateTime.Equal(now) {
		t.Errorf("UpdateTime = %v, want %v", run.UpdateTime, now)
	}
	wantParent := "runs/" + parentID.String()
	if run.Parent == nil || *run.Parent != wantParent {
		t.Errorf("Parent = %v, want %q", run.Parent, wantParent)
	}
	if run.SupportedActions == nil {
		t.Error("SupportedActions is nil, want non-nil")
	}
}
