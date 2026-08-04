package poller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/alfred/internal/config"
	autotask "github.com/tphakala/go-autotask"
	"github.com/tphakala/go-autotask/autotasktest"
	"github.com/tphakala/go-autotask/entities"
)

const (
	testEntityTicket = "Ticket"
	testFieldStatus  = "status"
	testStatusNew    = "New"
	testWorkflowName = "test-wf"
	testTriggerType  = "polling"
)

func TestMatchTrigger(t *testing.T) {
	trigger := config.TriggerConfig{
		Entity: testEntityTicket,
		Filter: map[string]any{testFieldStatus: testStatusNew},
	}
	ticket := map[string]any{testFieldStatus: testStatusNew, "title": "Test"}

	assert.True(t, matchesTrigger(trigger, ticket), "should match when status is New")

	ticket[testFieldStatus] = "Complete"
	assert.False(t, matchesTrigger(trigger, ticket), "should not match when status is Complete")
}

func TestMatchTrigger_EmptyFilter(t *testing.T) {
	trigger := config.TriggerConfig{
		Entity: testEntityTicket,
		Filter: map[string]any{},
	}
	ticket := map[string]any{testFieldStatus: testStatusNew}

	assert.True(t, matchesTrigger(trigger, ticket), "empty filter should match any ticket")
}

func TestMatchTrigger_MissingKey(t *testing.T) {
	trigger := config.TriggerConfig{
		Entity: testEntityTicket,
		Filter: map[string]any{"queueID": "5"},
	}
	ticket := map[string]any{testFieldStatus: testStatusNew} // no queueID

	assert.False(t, matchesTrigger(trigger, ticket), "should not match when filter key is absent")
}

func TestMatchTrigger_NumericComparison(t *testing.T) {
	trigger := config.TriggerConfig{
		Entity: testEntityTicket,
		Filter: map[string]any{testFieldStatus: int64(1)},
	}
	ticket := map[string]any{testFieldStatus: int64(1)}

	assert.True(t, matchesTrigger(trigger, ticket), "numeric values should match via Sprintf")

	ticket[testFieldStatus] = int64(2)
	assert.False(t, matchesTrigger(trigger, ticket))
}

func TestTicketToMap(t *testing.T) {
	tk := autotasktest.TicketFixture()
	m := ticketToMap(&tk)

	assert.Contains(t, m, "id")
	assert.Contains(t, m, "Title")
	assert.Contains(t, m, "Description")
	assert.Contains(t, m, testFieldStatus)
	assert.Contains(t, m, "priority")
	assert.Contains(t, m, "ticketNumber")
}

func TestTicketToMap_UnsetFieldsOmitted(t *testing.T) {
	tk := entities.Ticket{
		ID:    autotask.Set(int64(42)),
		Title: autotask.Set("Only title set"),
	}
	m := ticketToMap(&tk)

	assert.Equal(t, int64(42), m["id"])
	assert.Equal(t, "Only title set", m["Title"])
	assert.NotContains(t, m, testFieldStatus, "unset status should be absent")
	assert.NotContains(t, m, "priority", "unset priority should be absent")
}

func TestPollOnce(t *testing.T) {
	ticket := autotasktest.TicketFixture(func(tk *entities.Ticket) {
		tk.Status = autotask.Set(int64(1))
	})
	_, client := autotasktest.NewServer(t, autotasktest.WithEntity(ticket))

	workflows := []*config.WorkflowConfig{{
		Name: testWorkflowName,
		Trigger: config.TriggerConfig{
			Type:     testTriggerType,
			Entity:   testEntityTicket,
			Interval: time.Minute,
		},
	}}

	dispatched := make([]DispatchEvent, 0)
	dispatcher := func(event DispatchEvent) error {
		dispatched = append(dispatched, event)
		return nil
	}

	p := New(client, workflows, dispatcher)
	err := p.PollOnce(t.Context())

	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(dispatched), 1, "at least one event should be dispatched")
	assert.Equal(t, workflows[0], dispatched[0].WorkflowConfig)
}

func TestPollOnce_FilterMatch(t *testing.T) {
	ticket := autotasktest.TicketFixture(func(tk *entities.Ticket) {
		tk.Status = autotask.Set(int64(1))
	})
	_, client := autotasktest.NewServer(t, autotasktest.WithEntity(ticket))

	workflows := []*config.WorkflowConfig{
		{
			Name: "matching-wf",
			Trigger: config.TriggerConfig{
				Type:     testTriggerType,
				Entity:   testEntityTicket,
				Interval: time.Minute,
				Filter:   map[string]any{testFieldStatus: int64(1)},
			},
		},
		{
			Name: "non-matching-wf",
			Trigger: config.TriggerConfig{
				Type:     testTriggerType,
				Entity:   testEntityTicket,
				Interval: time.Minute,
				Filter:   map[string]any{testFieldStatus: int64(99)},
			},
		},
	}

	dispatched := make([]DispatchEvent, 0)
	dispatcher := func(event DispatchEvent) error {
		dispatched = append(dispatched, event)
		return nil
	}

	p := New(client, workflows, dispatcher)
	err := p.PollOnce(t.Context())

	require.NoError(t, err)
	require.Len(t, dispatched, 1, "only the matching workflow should fire")
	assert.Equal(t, "matching-wf", dispatched[0].WorkflowConfig.Name)
}

func TestShortestInterval(t *testing.T) {
	p := &Poller{
		workflows: []*config.WorkflowConfig{
			{Trigger: config.TriggerConfig{Interval: 10 * time.Minute}},
			{Trigger: config.TriggerConfig{Interval: 2 * time.Minute}},
			{Trigger: config.TriggerConfig{Interval: 30 * time.Minute}},
		},
	}
	assert.Equal(t, 2*time.Minute, p.shortestInterval())
}

func TestShortestInterval_DefaultWhenEmpty(t *testing.T) {
	p := &Poller{workflows: []*config.WorkflowConfig{}}
	assert.Equal(t, defaultInterval, p.shortestInterval())
}

func TestShortestInterval_DefaultWhenNoIntervals(t *testing.T) {
	p := &Poller{
		workflows: []*config.WorkflowConfig{
			{Trigger: config.TriggerConfig{Interval: 0}},
		},
	}
	assert.Equal(t, defaultInterval, p.shortestInterval())
}

func TestPollOnce_NoTickets(t *testing.T) {
	// Server with no seeded tickets.
	_, client := autotasktest.NewServer(t)

	workflows := []*config.WorkflowConfig{{
		Name: testWorkflowName,
		Trigger: config.TriggerConfig{
			Type:   testTriggerType,
			Entity: testEntityTicket,
		},
	}}

	dispatched := make([]DispatchEvent, 0)
	p := New(client, workflows, func(event DispatchEvent) error {
		dispatched = append(dispatched, event)
		return nil
	})

	err := p.PollOnce(t.Context())
	require.NoError(t, err)
	assert.Empty(t, dispatched, "no events should be dispatched when there are no tickets")
}

func TestPollOnce_NoMatchingWorkflows(t *testing.T) {
	ticket := autotasktest.TicketFixture(func(tk *entities.Ticket) {
		tk.Status = autotask.Set(int64(1))
	})
	_, client := autotasktest.NewServer(t, autotasktest.WithEntity(ticket))

	workflows := []*config.WorkflowConfig{{
		Name: "high-priority-wf",
		Trigger: config.TriggerConfig{
			Type:   testTriggerType,
			Entity: testEntityTicket,
			Filter: map[string]any{testFieldStatus: int64(99)}, // no ticket has status 99
		},
	}}

	dispatched := make([]DispatchEvent, 0)
	p := New(client, workflows, func(event DispatchEvent) error {
		dispatched = append(dispatched, event)
		return nil
	})

	err := p.PollOnce(t.Context())
	require.NoError(t, err)
	assert.Empty(t, dispatched, "no events should be dispatched when no tickets match the filter")
}

func TestPollOnce_DispatchErrorContinues(t *testing.T) {
	// Two tickets; dispatcher errors on the first but should still process the second.
	t1 := autotasktest.TicketFixture(func(tk *entities.Ticket) {
		tk.Status = autotask.Set(int64(1))
	})
	t2 := autotasktest.TicketFixture(func(tk *entities.Ticket) {
		tk.Status = autotask.Set(int64(1))
	})
	_, client := autotasktest.NewServer(t, autotasktest.WithEntity(t1, t2))

	workflows := []*config.WorkflowConfig{{
		Name: testWorkflowName,
		Trigger: config.TriggerConfig{
			Type:   testTriggerType,
			Entity: testEntityTicket,
		},
	}}

	callCount := 0
	dispatcher := func(event DispatchEvent) error {
		callCount++
		if callCount == 1 {
			return fmt.Errorf("dispatch error")
		}
		return nil
	}

	p := New(client, workflows, dispatcher)
	err := p.PollOnce(t.Context())

	// PollOnce itself should succeed (dispatch errors are logged, not propagated).
	require.NoError(t, err)
	assert.Equal(t, 2, callCount, "dispatcher should be called for both tickets")
}

func TestRun_CancelledContext(t *testing.T) {
	_, client := autotasktest.NewServer(t)
	p := New(client, nil, func(DispatchEvent) error { return nil })

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	err := p.Run(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestPoller_Status(t *testing.T) {
	p := New(nil, nil, nil)
	status := p.Status()

	if !status.LastPolled.IsZero() {
		t.Errorf("expected zero LastPolled before first poll, got %v", status.LastPolled)
	}
	if status.SeenCount != 0 {
		t.Errorf("expected 0 SeenCount, got %d", status.SeenCount)
	}
}
