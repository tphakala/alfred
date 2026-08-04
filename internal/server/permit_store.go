package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"time"
)

// Approval decision strings used by ResolveApproval and the session policy.
const (
	decisionAllow = "allow"
	decisionDeny  = "deny"
)

// ApprovalResolution is the decision for a pending permission request.
type ApprovalResolution struct {
	Decision      string          `json:"decision"`
	ModifiedInput json.RawMessage `json:"modified_input,omitempty"`
	Reason        string          `json:"reason,omitempty"`
}

// SessionApprovalPolicy controls auto-approve behavior for a run.
type SessionApprovalPolicy struct {
	AutoApproveAll   bool            `json:"auto_approve_all"`
	AutoApproveTools map[string]bool `json:"auto_approve_tools,omitempty"`
	DenyTools        map[string]bool `json:"deny_tools,omitempty"`
}

// SessionApprovalPolicyDelta is an additive update to a session policy.
type SessionApprovalPolicyDelta struct {
	AddTools []string `json:"add_tools,omitempty"`
}

type runEntry struct {
	token    string
	expiry   time.Time
	channels map[string]chan ApprovalResolution
	policy   SessionApprovalPolicy
}

// PermitStore manages per-run bearer tokens, approval channels, and session
// approval policies. All state is in-process; it dies with the Alfred process.
type PermitStore struct {
	mu   sync.Mutex
	runs map[string]*runEntry
}

// NewPermitStore creates a new PermitStore ready for use.
func NewPermitStore() *PermitStore {
	return &PermitStore{runs: make(map[string]*runEntry)}
}

// RegisterRun creates a new run entry with a bearer token that expires after ttl.
// Returns the generated token.
func (ps *PermitStore) RegisterRun(runID string, ttl time.Duration) string {
	token := generateToken()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.runs[runID] = &runEntry{
		token:    token,
		expiry:   time.Now().Add(ttl),
		channels: make(map[string]chan ApprovalResolution),
	}
	return token
}

// DeregisterRun removes a run entry, sending a "deny" resolution to any
// pending approval channels so blocked MCP handlers get a clean response
// instead of a zero-value from a closed channel.
func (ps *PermitStore) DeregisterRun(runID string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if entry, ok := ps.runs[runID]; ok {
		for _, ch := range entry.channels {
			select {
			case ch <- ApprovalResolution{Decision: decisionDeny, Reason: "run ended"}:
			default:
			}
		}
		delete(ps.runs, runID)
	}
}

// ValidateToken checks whether the given token matches the registered token for
// the run. Returns false if the run does not exist or the token has expired.
// Uses constant-time comparison to prevent timing attacks.
func (ps *PermitStore) ValidateToken(runID, token string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	entry, ok := ps.runs[runID]
	if !ok {
		return false
	}
	if time.Now().After(entry.expiry) {
		for _, ch := range entry.channels {
			select {
			case ch <- ApprovalResolution{Decision: decisionDeny, Reason: "token expired"}:
			default:
			}
		}
		delete(ps.runs, runID)
		return false
	}
	return subtle.ConstantTimeCompare([]byte(entry.token), []byte(token)) == 1
}

// CreateApprovalChannel creates a buffered channel for receiving the approval
// resolution for a specific call within a run. Returns nil if the run does not
// exist.
func (ps *PermitStore) CreateApprovalChannel(runID, callID string) chan ApprovalResolution {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	entry, ok := ps.runs[runID]
	if !ok {
		return nil
	}
	ch := make(chan ApprovalResolution, 1)
	entry.channels[callID] = ch
	return ch
}

// RemoveApprovalChannel removes an orphaned approval channel (e.g. when the
// HTTP request is cancelled before a resolution arrives).
func (ps *PermitStore) RemoveApprovalChannel(runID, callID string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if entry, ok := ps.runs[runID]; ok {
		delete(entry.channels, callID)
	}
}

// ResolveApproval sends a resolution to the approval channel for the given call.
// Returns false if the run or channel does not exist. The channel is removed
// after resolution.
func (ps *PermitStore) ResolveApproval(runID, callID string, res ApprovalResolution) bool {
	ps.mu.Lock()
	entry, ok := ps.runs[runID]
	if !ok {
		ps.mu.Unlock()
		return false
	}
	ch, ok := entry.channels[callID]
	if !ok {
		ps.mu.Unlock()
		return false
	}
	delete(entry.channels, callID)
	ps.mu.Unlock()
	ch <- res
	return true
}

// PendingApprovals returns the call IDs that currently have an open approval
// channel for the run (i.e. permission requests awaiting resolution). Returns
// nil if the run is unknown.
func (ps *PermitStore) PendingApprovals(runID string) []string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	entry, ok := ps.runs[runID]
	if !ok {
		return nil
	}
	return slices.Collect(maps.Keys(entry.channels))
}

// GetSessionPolicy returns the current session approval policy for a run.
// Returns a zero-value policy if the run does not exist.
func (ps *PermitStore) GetSessionPolicy(runID string) SessionApprovalPolicy {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	entry, ok := ps.runs[runID]
	if !ok {
		return SessionApprovalPolicy{}
	}
	return entry.policy
}

// SetSessionPolicy replaces the session approval policy for a run.
func (ps *PermitStore) SetSessionPolicy(runID string, policy SessionApprovalPolicy) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if entry, ok := ps.runs[runID]; ok {
		entry.policy = policy
	}
}

// ExtendSessionPolicy applies an additive delta to the session approval policy,
// adding tools to the auto-approve set.
func (ps *PermitStore) ExtendSessionPolicy(runID string, delta SessionApprovalPolicyDelta) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	entry, ok := ps.runs[runID]
	if !ok {
		return
	}
	if entry.policy.AutoApproveTools == nil {
		entry.policy.AutoApproveTools = make(map[string]bool)
	}
	for _, tool := range delta.AddTools {
		entry.policy.AutoApproveTools[tool] = true
	}
}

// CheckPolicy evaluates the session policy for a tool call. Returns "allow" if
// auto-approved, "deny" if explicitly denied, or "" if no policy matches
// (meaning the caller should prompt the user).
func (ps *PermitStore) CheckPolicy(runID, toolName string, _ json.RawMessage) string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	entry, ok := ps.runs[runID]
	if !ok {
		return ""
	}
	if entry.policy.DenyTools[toolName] {
		return decisionDeny
	}
	if entry.policy.AutoApproveAll {
		return decisionAllow
	}
	if entry.policy.AutoApproveTools[toolName] {
		return decisionAllow
	}
	return ""
}

func generateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
}
