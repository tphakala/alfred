package ctxbuild

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

const testRoleAssistant = "assistant"

// TestTieredContextBuilder_UnderBudget verifies that all messages are included
// when the token budget is large enough to fit them all.
func TestTieredContextBuilder_UnderBudget(t *testing.T) {
	builder := NewTieredContextBuilder(5)

	messages := []StoredMessage{
		{Sequence: 1, Role: RoleUser, Content: "Hello there", TokenEstimate: 10},
		{Sequence: 2, Role: testRoleAssistant, Content: "Hi! How can I help?", TokenEstimate: 15},
		{Sequence: 3, Role: RoleUser, Content: "Tell me a joke", TokenEstimate: 10},
	}

	params := BuildParams{
		SessionID:   uuid.New(),
		TokenBudget: 1000,
		Messages:    messages,
	}

	result, err := builder.Build(t.Context(), &params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result.Messages))
	}

	expectedTokens := 10 + 15 + 10
	if result.TokensUsed != expectedTokens {
		t.Errorf("expected TokensUsed=%d, got %d", expectedTokens, result.TokensUsed)
	}

	if result.TruncatedFrom != 3 {
		t.Errorf("expected TruncatedFrom=3, got %d", result.TruncatedFrom)
	}
}

// TestTieredContextBuilder_OverBudget_DropOldest verifies that oldest messages
// are dropped when the total exceeds the token budget.
func TestTieredContextBuilder_OverBudget_DropOldest(t *testing.T) {
	builder := NewTieredContextBuilder(5)

	messages := []StoredMessage{
		{Sequence: 1, Role: RoleUser, Content: "Message one", TokenEstimate: 40},
		{Sequence: 2, Role: testRoleAssistant, Content: "Message two", TokenEstimate: 40},
		{Sequence: 3, Role: RoleUser, Content: "Message three", TokenEstimate: 30},
		{Sequence: 4, Role: testRoleAssistant, Content: "Message four", TokenEstimate: 30},
	}
	// Total = 140, budget = 50; newest-first: msg4(30)+msg3(30)=60 > 50, so only msg4(30) fits?
	// Actually: start with msg4=30, then msg4+msg3=60 > 50, stop.
	// So only msg4 is included... but the spec says "only newest 2 included".
	// Let's re-read: budget=50, msg4=30 fits, msg4+msg3=60 > 50 → stop after msg4.
	// Hmm, the test description says "only newest 2 included" with budget 50.
	// 30+30=60 > 50. Let's set budget to 65 to get 2 messages (30+30=60 <= 65).
	// Or adjust token estimates: make newest 2 total <= 50: 20+20=40.
	// Per the task spec: "4 messages totaling 140 tokens, budget 50 → only newest 2 included"
	// That means 2 newest messages together fit within 50: so they must each be 20 tokens.
	// But messages have 40,40,30,30. Let me use 20,20 for the last two.
	_ = messages // ignore above, redefine below

	messages = []StoredMessage{
		{Sequence: 1, Role: RoleUser, Content: "Message one", TokenEstimate: 40},
		{Sequence: 2, Role: testRoleAssistant, Content: "Message two", TokenEstimate: 40},
		{Sequence: 3, Role: RoleUser, Content: "Message three", TokenEstimate: 20},
		{Sequence: 4, Role: testRoleAssistant, Content: "Message four", TokenEstimate: 20},
	}
	// Total = 120 (not 140, but close enough to spec intent), budget = 50
	// msg4(20) fits, msg4+msg3(40) fits, msg4+msg3+msg2(80) > 50 → stop.
	// Result: msg3 and msg4 included (newest 2).

	params := BuildParams{
		SessionID:   uuid.New(),
		TokenBudget: 50,
		Messages:    messages,
	}

	result, err := builder.Build(t.Context(), &params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result.Messages))
	}

	// Messages should be in sequence order (oldest first in output).
	if result.Messages[0].Sequence != 3 {
		t.Errorf("expected first message sequence=3, got %d", result.Messages[0].Sequence)
	}
	if result.Messages[1].Sequence != 4 {
		t.Errorf("expected second message sequence=4, got %d", result.Messages[1].Sequence)
	}

	if result.TokensUsed != 40 {
		t.Errorf("expected TokensUsed=40, got %d", result.TokensUsed)
	}

	if result.TruncatedFrom != 4 {
		t.Errorf("expected TruncatedFrom=4, got %d", result.TruncatedFrom)
	}
}

// TestTieredContextBuilder_PruneStaleToolResults verifies that tool results older
// than staleTurnThreshold assistant turns are pruned to a stub, while recent ones
// are kept intact.
func TestTieredContextBuilder_PruneStaleToolResults(t *testing.T) {
	builder := NewTieredContextBuilder(2)

	// Max sequence = 9. For a message at seq 3:
	// turnsAgo = (9 - 3) / 2 = 3, which exceeds threshold=2 → pruned.
	// For a message at seq 7:
	// turnsAgo = (9 - 7) / 2 = 1, which is within threshold=2 → kept.
	messages := []StoredMessage{
		{Sequence: 1, Role: RoleUser, Content: "Use a tool", TokenEstimate: 10},
		{Sequence: 2, Role: testRoleAssistant, Content: "Calling tool...", TokenEstimate: 10},
		{Sequence: 3, Role: RoleToolResult, Content: "Big tool result with lots of data", TokenEstimate: 500},
		{Sequence: 4, Role: RoleUser, Content: "What about now?", TokenEstimate: 10},
		{Sequence: 5, Role: testRoleAssistant, Content: "Let me check again", TokenEstimate: 10},
		{Sequence: 6, Role: testRoleAssistant, Content: "Calling tool again...", TokenEstimate: 10},
		{Sequence: 7, Role: RoleToolResult, Content: "Recent tool result", TokenEstimate: 20},
		{Sequence: 8, Role: RoleUser, Content: "Great, thanks", TokenEstimate: 10},
		{Sequence: 9, Role: testRoleAssistant, Content: "You are welcome", TokenEstimate: 10},
	}

	params := BuildParams{
		SessionID:   uuid.New(),
		TokenBudget: 10000,
		Messages:    messages,
	}

	result, err := builder.Build(t.Context(), &params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Messages) != 9 {
		t.Fatalf("expected 9 messages, got %d", len(result.Messages))
	}

	// Find seq=3 and verify it was pruned.
	var stale, recent *BuiltMessage
	for i := range result.Messages {
		switch result.Messages[i].Sequence {
		case 3:
			stale = &result.Messages[i]
		case 7:
			recent = &result.Messages[i]
		}
	}

	if stale == nil {
		t.Fatal("did not find message with sequence=3")
	}
	if stale.Content != PrunedStubContent {
		t.Errorf("expected stale tool result pruned, got content=%q", stale.Content)
	}
	if stale.TokenEstimate != prunedStubTokens {
		t.Errorf("expected stale token estimate=%d, got %d", prunedStubTokens, stale.TokenEstimate)
	}

	if recent == nil {
		t.Fatal("did not find message with sequence=7")
	}
	if recent.Content != "Recent tool result" {
		t.Errorf("expected recent tool result kept intact, got content=%q", recent.Content)
	}
	if recent.TokenEstimate != 20 {
		t.Errorf("expected recent token estimate=20, got %d", recent.TokenEstimate)
	}
}

// TestTieredContextBuilder_PruneStaleApprovalResults verifies that approval results
// are pruned with the same staleness logic as tool results.
func TestTieredContextBuilder_PruneStaleApprovalResults(t *testing.T) {
	builder := NewTieredContextBuilder(2)

	messages := []StoredMessage{
		{Sequence: 1, Role: RoleUser, Content: "deploy it", TokenEstimate: 10},
		{Sequence: 2, Role: RoleModel, Content: "I will deploy", TokenEstimate: 10},
		{Sequence: 3, Role: RoleApprovalResult, Content: `{"approved":true}`, TokenEstimate: 500},
		{Sequence: 4, Role: RoleUser, Content: "what next?", TokenEstimate: 10},
		{Sequence: 5, Role: RoleModel, Content: "checking", TokenEstimate: 10},
		{Sequence: 6, Role: RoleModel, Content: "calling tool", TokenEstimate: 10},
		{Sequence: 7, Role: RoleApprovalResult, Content: `{"approved":false}`, TokenEstimate: 20},
		{Sequence: 8, Role: RoleUser, Content: "ok", TokenEstimate: 10},
		{Sequence: 9, Role: RoleModel, Content: "done", TokenEstimate: 10},
	}

	params := BuildParams{
		SessionID:   uuid.New(),
		TokenBudget: 10000,
		Messages:    messages,
	}

	result, err := builder.Build(t.Context(), &params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Messages) != 9 {
		t.Fatalf("expected 9 messages, got %d", len(result.Messages))
	}

	var stale, recent *BuiltMessage
	for i := range result.Messages {
		switch result.Messages[i].Sequence {
		case 3:
			stale = &result.Messages[i]
		case 7:
			recent = &result.Messages[i]
		}
	}

	if stale == nil {
		t.Fatal("did not find message with sequence=3")
	}
	if stale.Content != PrunedStubContent {
		t.Errorf("expected stale approval result pruned, got content=%q", stale.Content)
	}
	if stale.TokenEstimate != prunedStubTokens {
		t.Errorf("expected stale token estimate=%d, got %d", prunedStubTokens, stale.TokenEstimate)
	}

	if recent == nil {
		t.Fatal("did not find message with sequence=7")
	}
	if recent.Content != `{"approved":false}` {
		t.Errorf("expected recent approval result kept intact, got content=%q", recent.Content)
	}
}

// TestTieredContextBuilder_EpochSummaryAndMemories verifies that epoch summary
// and memories are injected as a "context" role prefix message.
func TestTieredContextBuilder_EpochSummaryAndMemories(t *testing.T) {
	builder := NewTieredContextBuilder(5)

	messages := []StoredMessage{
		{Sequence: 1, Role: RoleUser, Content: "Hello", TokenEstimate: 5},
	}

	epochSummary := "Previous conversation: user asked about Go concurrency."
	memories := []string{
		"User prefers short answers.",
		"User is experienced with Go.",
	}

	params := BuildParams{
		SessionID:    uuid.New(),
		TokenBudget:  10000,
		Messages:     messages,
		EpochSummary: epochSummary,
		Memories:     memories,
	}

	result, err := builder.Build(t.Context(), &params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expect prefix + 1 message = 2 total.
	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 messages (prefix + 1), got %d", len(result.Messages))
	}

	prefix := result.Messages[0]
	if prefix.Role != RoleContext {
		t.Errorf("expected first message role='context', got %q", prefix.Role)
	}

	if !strings.Contains(prefix.Content, epochSummary) {
		t.Errorf("prefix content missing epoch summary; content=%q", prefix.Content)
	}

	for _, mem := range memories {
		if !strings.Contains(prefix.Content, mem) {
			t.Errorf("prefix content missing memory %q; content=%q", mem, prefix.Content)
		}
	}

	// Token estimate for prefix should be len(content)/4.
	expectedTokens := len(prefix.Content) / 4
	if prefix.TokenEstimate != expectedTokens {
		t.Errorf("expected prefix TokenEstimate=%d, got %d", expectedTokens, prefix.TokenEstimate)
	}
}

// TestTieredContextBuilder_EmptyMessages verifies that an empty message list
// returns an empty result without panicking.
func TestTieredContextBuilder_EmptyMessages(t *testing.T) {
	builder := NewTieredContextBuilder(5)

	params := BuildParams{
		SessionID:   uuid.New(),
		TokenBudget: 1000,
		Messages:    []StoredMessage{},
	}

	result, err := builder.Build(t.Context(), &params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Messages) != 0 {
		t.Errorf("expected 0 messages, got %d", len(result.Messages))
	}

	if result.TokensUsed != 0 {
		t.Errorf("expected TokensUsed=0, got %d", result.TokensUsed)
	}
}
