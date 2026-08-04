package epoch

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

const testKindChat = "chat"

func TestComposableEpochManager_NoTransition(t *testing.T) {
	mgr := NewComposableEpochManager(ManagerConfig{
		TokenThreshold: 0.80,
		MaxDuration:    4 * time.Hour,
	})

	now := time.Now()
	state := EpochState{
		SessionID:      uuid.New(),
		Kind:           testKindChat,
		Epoch:          1,
		TokensUsed:     10_000,
		TokenBudget:    100_000,
		MessageCount:   5,
		EpochStartedAt: now.Add(-30 * time.Minute),
		LastMessageAt:  now,
		Now:            now,
	}

	result := mgr.ShouldTransition(t.Context(), &state)
	if result != nil {
		t.Errorf("expected nil, got %+v", result)
	}
}

func TestComposableEpochManager_TokenThreshold(t *testing.T) {
	mgr := NewComposableEpochManager(ManagerConfig{
		TokenThreshold: 0.80,
		MaxDuration:    4 * time.Hour,
	})

	now := time.Now()
	state := EpochState{
		SessionID:      uuid.New(),
		Kind:           testKindChat,
		Epoch:          1,
		TokensUsed:     85_000,
		TokenBudget:    100_000,
		MessageCount:   20,
		EpochStartedAt: now.Add(-30 * time.Minute),
		LastMessageAt:  now,
		Now:            now,
	}

	result := mgr.ShouldTransition(t.Context(), &state)
	if result == nil {
		t.Fatal("expected transition, got nil")
	}
	if result.Trigger != TriggerTokenThreshold {
		t.Errorf("expected trigger %q, got %q", TriggerTokenThreshold, result.Trigger)
	}
}

func TestComposableEpochManager_TimeBased(t *testing.T) {
	mgr := NewComposableEpochManager(ManagerConfig{
		TokenThreshold: 0.80,
		MaxDuration:    4 * time.Hour,
	})

	now := time.Now()
	state := EpochState{
		SessionID:      uuid.New(),
		Kind:           "monitor",
		Epoch:          2,
		TokensUsed:     10_000,
		TokenBudget:    100_000,
		MessageCount:   10,
		EpochStartedAt: now.Add(-5 * time.Hour),
		LastMessageAt:  now,
		Now:            now,
	}

	result := mgr.ShouldTransition(t.Context(), &state)
	if result == nil {
		t.Fatal("expected transition, got nil")
	}
	if result.Trigger != TriggerTime {
		t.Errorf("expected trigger %q, got %q", TriggerTime, result.Trigger)
	}
}

func TestComposableEpochManager_FirstMatchWins(t *testing.T) {
	mgr := NewComposableEpochManager(ManagerConfig{
		TokenThreshold: 0.80,
		MaxDuration:    4 * time.Hour,
	})

	// Both token (85K/100K > 80%) and time (5h > 4h) would trigger.
	// Token threshold is checked first, so it should win.
	now := time.Now()
	state := EpochState{
		SessionID:      uuid.New(),
		Kind:           "ticket",
		Epoch:          3,
		TokensUsed:     85_000,
		TokenBudget:    100_000,
		MessageCount:   30,
		EpochStartedAt: now.Add(-5 * time.Hour),
		LastMessageAt:  now,
		Now:            now,
	}

	result := mgr.ShouldTransition(t.Context(), &state)
	if result == nil {
		t.Fatal("expected transition, got nil")
	}
	if result.Trigger != TriggerTokenThreshold {
		t.Errorf("expected trigger %q, got %q", TriggerTokenThreshold, result.Trigger)
	}
}

func TestComposableEpochManager_ZeroConfig(t *testing.T) {
	mgr := NewComposableEpochManager(ManagerConfig{})

	now := time.Now()
	state := EpochState{
		SessionID:      uuid.New(),
		Kind:           testKindChat,
		Epoch:          1,
		TokensUsed:     999_999,
		TokenBudget:    100_000,
		MessageCount:   100,
		EpochStartedAt: now.Add(-100 * time.Hour),
		LastMessageAt:  now,
		Now:            now,
	}

	// Zero config means neither trigger is active — must not panic and must return nil.
	result := mgr.ShouldTransition(t.Context(), &state)
	if result != nil {
		t.Errorf("expected nil with zero config, got %+v", result)
	}
}
