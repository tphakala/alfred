// Package poller polls the Autotask API for tickets and dispatches matching
// workflow events.
package poller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tphakala/alfred/internal/config"
	autotask "github.com/tphakala/go-autotask"
	"github.com/tphakala/go-autotask/entities"
)

const (
	defaultInterval = 5 * time.Minute
	ticketLimit     = 500
)

// PollerStatus holds the current state of the poller for status reporting.
type PollerStatus struct {
	LastPolled    time.Time `json:"lastPolled"`
	SeenCount     int       `json:"seenCount"`
	WorkflowCount int       `json:"workflowCount"`
}

// DispatchEvent carries the data for a single ticket-workflow match.
type DispatchEvent struct {
	TicketID       int64
	TicketData     map[string]any
	WorkflowConfig *config.WorkflowConfig
}

// DispatchFunc is called for each ticket that matches a workflow trigger.
type DispatchFunc func(event DispatchEvent) error

// Poller periodically queries the Autotask API and dispatches matching events.
type Poller struct {
	mu         sync.RWMutex
	client     *autotask.Client
	workflows  []*config.WorkflowConfig
	dispatch   DispatchFunc
	logger     *slog.Logger
	seen       map[string]bool
	lastPolled time.Time
}

// New creates a new Poller. The dispatch function is called synchronously in
// PollOnce for each ticket/workflow combination that matches.
func New(client *autotask.Client, workflows []*config.WorkflowConfig, dispatch DispatchFunc) *Poller {
	return &Poller{
		client:    client,
		workflows: workflows,
		dispatch:  dispatch,
		logger:    slog.Default(),
		seen:      make(map[string]bool),
	}
}

// Status returns the current poller state. Safe to call concurrently from
// the HTTP server goroutine.
func (p *Poller) Status() PollerStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return PollerStatus{
		LastPolled:    p.lastPolled,
		SeenCount:     len(p.seen),
		WorkflowCount: len(p.workflows),
	}
}

// Run starts the polling loop. It calculates the shortest interval across all
// configured workflows, then ticks at that interval and calls PollOnce each
// time. It blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) error {
	interval := p.shortestInterval()
	p.logger.Info("poller starting", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("poller stopping", "reason", ctx.Err())
			return ctx.Err()
		case <-ticker.C:
			if err := p.PollOnce(ctx); err != nil {
				p.logger.Error("poll cycle error", "error", err)
			}
		}
	}
}

// PollOnce queries recently-modified tickets and dispatches a DispatchEvent for
// every ticket/workflow pair whose trigger filter matches the ticket fields.
// On first poll all tickets are fetched; subsequent polls filter by
// lastActivityDate > lastPolled to reduce API load. Duplicate dispatches within
// the lifetime of the Poller are suppressed via the seen map.
func (p *Poller) PollOnce(ctx context.Context) error {
	q := autotask.NewQuery().Limit(ticketLimit)
	p.mu.RLock()
	lastPolled := p.lastPolled
	p.mu.RUnlock()
	if !lastPolled.IsZero() {
		q.Where("lastActivityDate", autotask.OpGt, lastPolled.Format(time.RFC3339))
	}

	tickets, err := autotask.List[entities.Ticket](ctx, p.client, q)
	if err != nil {
		return fmt.Errorf("poller: list tickets: %w", err)
	}

	p.mu.Lock()
	p.lastPolled = time.Now()
	p.mu.Unlock()
	p.logger.Debug("polled tickets", "count", len(tickets))

	for _, ticket := range tickets {
		ticketMap := ticketToMap(ticket)
		id, _ := ticket.ID.Get()

		for _, wf := range p.workflows {
			if !matchesTrigger(wf.Trigger, ticketMap) {
				continue
			}

			key := fmt.Sprintf("%s-%d", wf.Name, id)
			p.mu.Lock()
			if p.seen[key] {
				p.mu.Unlock()
				p.logger.Debug("skipping duplicate dispatch", "ticket_id", id, "workflow", wf.Name)
				continue
			}
			p.seen[key] = true
			p.mu.Unlock()

			event := DispatchEvent{
				TicketID:       id,
				TicketData:     ticketMap,
				WorkflowConfig: wf,
			}
			if err := p.dispatch(event); err != nil {
				p.logger.Error("dispatch error", "ticket_id", id, "workflow", wf.Name, "error", err)
			}
		}
	}

	return nil
}

// shortestInterval returns the minimum polling interval configured across all
// workflows. Returns defaultInterval (5m) if no workflows are configured or
// none specify an interval.
func (p *Poller) shortestInterval() time.Duration {
	shortest := defaultInterval
	for _, wf := range p.workflows {
		if wf.Trigger.Interval > 0 && wf.Trigger.Interval < shortest {
			shortest = wf.Trigger.Interval
		}
	}
	return shortest
}

// matchesTrigger reports whether all entries in trigger.Filter match the
// corresponding keys in ticket. Both actual and expected values are compared
// as fmt.Sprintf("%v", ...) strings.
func matchesTrigger(trigger config.TriggerConfig, ticket map[string]any) bool {
	for key, expected := range trigger.Filter {
		actual, ok := ticket[key]
		if !ok {
			return false
		}
		if fmt.Sprintf("%v", actual) != fmt.Sprintf("%v", expected) {
			return false
		}
	}
	return true
}

// ticketToMap extracts the most useful fields from a Ticket into a plain map.
// Optional fields that are not set are omitted.
func ticketToMap(t *entities.Ticket) map[string]any {
	m := make(map[string]any)

	if v, ok := t.ID.Get(); ok {
		m["id"] = v
	}
	if v, ok := t.Title.Get(); ok {
		m["Title"] = v
	}
	if v, ok := t.Description.Get(); ok {
		m["Description"] = v
	}
	if v, ok := t.Status.Get(); ok {
		m["status"] = v
	}
	if v, ok := t.Priority.Get(); ok {
		m["priority"] = v
	}
	if v, ok := t.QueueID.Get(); ok {
		m["queueID"] = v
	}
	if v, ok := t.TicketNumber.Get(); ok {
		m["ticketNumber"] = v
	}

	return m
}
