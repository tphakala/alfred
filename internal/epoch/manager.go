package epoch

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// EpochState represents the current state of an epoch for boundary evaluation.
type EpochState struct {
	SessionID      uuid.UUID
	Kind           string // "chat", "monitor", "ticket"
	Epoch          int
	TokensUsed     int
	TokenBudget    int
	MessageCount   int
	EpochStartedAt time.Time
	LastMessageAt  time.Time
	Now            time.Time // deterministic "now" — callers inside Temporal must use workflow.Now(ctx)
}

// EpochTransition describes why an epoch boundary was triggered.
type EpochTransition struct {
	Trigger string // "token_threshold", "time", "user_initiated", "task_complete"
}

// Trigger constants for EpochTransition.
const (
	TriggerTokenThreshold = "token_threshold"
	TriggerTime           = "time"
)

// EpochBoundaryManager determines if an epoch should transition.
type EpochBoundaryManager interface {
	ShouldTransition(ctx context.Context, state *EpochState) *EpochTransition
}

// ManagerConfig configures the ComposableEpochManager triggers.
type ManagerConfig struct {
	TokenThreshold float64       // e.g. 0.80 = fire at 80% of budget
	MaxDuration    time.Duration // e.g. 4h = fire after 4 hours
}

// ComposableEpochManager checks triggers in order, returns first match.
type ComposableEpochManager struct {
	cfg ManagerConfig
}

// NewComposableEpochManager creates a new ComposableEpochManager with the given config.
func NewComposableEpochManager(cfg ManagerConfig) *ComposableEpochManager {
	return &ComposableEpochManager{cfg: cfg}
}

// ShouldTransition evaluates epoch transition triggers in priority order and
// returns the first matching trigger, or nil if no transition is warranted.
//
// Trigger evaluation order:
//  1. Token threshold: fires when tokens used exceeds the configured fraction of the budget.
//  2. Time-based: fires when the epoch has been running longer than MaxDuration.
func (m *ComposableEpochManager) ShouldTransition(_ context.Context, state *EpochState) *EpochTransition {
	// 1. Token threshold check.
	if state.TokenBudget > 0 && m.cfg.TokenThreshold > 0 {
		if state.TokensUsed > int(float64(state.TokenBudget)*m.cfg.TokenThreshold) {
			return &EpochTransition{Trigger: TriggerTokenThreshold}
		}
	}

	// 2. Time-based check.
	if m.cfg.MaxDuration > 0 && !state.EpochStartedAt.IsZero() && !state.Now.IsZero() {
		if state.Now.Sub(state.EpochStartedAt) > m.cfg.MaxDuration {
			return &EpochTransition{Trigger: TriggerTime}
		}
	}

	return nil
}
