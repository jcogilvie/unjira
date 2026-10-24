// Package attribution turns the token ledger's read-time attribution into per-ticket
// state and defines the Sink that drafts writes from it.
//
// A sink only drafts, and never writes (D-31): it returns actions for the queue, and
// the gate's applier is the only party that reaches a tracker.
package attribution

import (
	"maps"
	"slices"
	"time"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/store"
)

// TicketState is everything a sink sees about one issue.
type TicketState struct {
	IssueKey string
	Unlogged []store.MessageAttribution            // tracked here, no marker
	Logged   map[string][]store.MessageAttribution // worklog id -> still tracked here
	Moved    map[string][]store.MessageAttribution // worklog id -> logged here, now not
	Actions  []store.ActionRow                     // every action on IssueKey
}

// DraftContext is the per-issue configuration a sink drafts under.
type DraftContext struct {
	Tracker   config.Tracker
	EnabledAt time.Time
}

// Draft is one write a sink proposes. Supersedes is the id of a pending action this
// replaces; Row nil with Supersedes set retires that action with no replacement.
type Draft struct {
	Row        *store.ActionRow
	Supersedes int64
}

// Sink drafts actions from one ticket's state.
type Sink interface {
	Name() string
	Draft(dc DraftContext, st TicketState) ([]Draft, error)
}

// ModelTotal is one model's summed token counts and their cost.
type ModelTotal struct {
	Input, Output, CacheWrite, CacheRead int64
	Cost                                 float64
	Price                                config.ModelPrice
}

// BuildTicketStates groups msgs into one state per issue, sorted by IssueKey. A logged
// message belongs to the ticket it is logged on: Logged while still tracked there, Moved
// otherwise, and in nothing on the ticket it now tracks to until that correction applies
// (R7). An unlogged message counts only when tracked. actions is keyed by issue key and
// attached as given.
func BuildTicketStates(msgs []store.MessageAttribution, actions map[string][]store.ActionRow) []TicketState {
	states := map[string]*TicketState{}
	get := func(key string) *TicketState {
		st, ok := states[key]
		if !ok {
			st = &TicketState{IssueKey: key, Actions: actions[key]}
			states[key] = st
		}
		return st
	}
	add := func(m *map[string][]store.MessageAttribution, id string, msg store.MessageAttribution) {
		if *m == nil {
			*m = map[string][]store.MessageAttribution{}
		}
		(*m)[id] = append((*m)[id], msg)
	}
	for _, m := range msgs {
		tracked := m.State == store.AttributionTracked
		switch {
		case m.WorklogID != "":
			st := get(m.WorklogIssueKey)
			if tracked && m.IssueKey == m.WorklogIssueKey {
				add(&st.Logged, m.WorklogID, m)
			} else {
				add(&st.Moved, m.WorklogID, m)
			}
		case tracked:
			st := get(m.IssueKey)
			st.Unlogged = append(st.Unlogged, m)
		}
	}
	out := make([]TicketState, 0, len(states))
	for _, key := range slices.Sorted(maps.Keys(states)) {
		out = append(out, *states[key])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Totals sums msgs per model and prices each kind as count·rate/1e6, the same formula
// as the token report. A model with no price is an error.
func Totals(msgs []store.MessageAttribution, pricing config.TokenPricing) (map[string]ModelTotal, error) {
	out := map[string]ModelTotal{}
	for _, m := range msgs {
		price, err := pricing.Lookup(m.Model)
		if err != nil {
			return nil, err
		}
		t := out[m.Model]
		t.Input += m.InputTokens
		t.Output += m.OutputTokens
		t.CacheWrite += m.CacheWriteTokens
		t.CacheRead += m.CacheReadTokens
		t.Price = price
		out[m.Model] = t
	}
	for model, t := range out {
		p := t.Price
		t.Cost = (float64(t.Input)**p.Input + float64(t.Output)**p.Output +
			float64(t.CacheWrite)**p.CacheWrite + float64(t.CacheRead)**p.CacheRead) / 1e6
		out[model] = t
	}
	return out, nil
}
