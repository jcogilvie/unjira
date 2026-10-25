package worklog

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/attribution"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/store"
)

// Sink drafts Jira worklogs: one add per ticket over its unlogged delta, and one
// correction per worklog whose messages moved elsewhere.
type Sink struct {
	cfg     config.WorklogConfig
	pricing config.TokenPricing
}

var _ attribution.Sink = (*Sink)(nil)

// New returns the worklog sink.
func New(cfg config.WorklogConfig, pricing config.TokenPricing) *Sink {
	return &Sink{cfg: cfg, pricing: pricing}
}

// Name is the sink's config name.
func (s *Sink) Name() string { return config.SinkWorklog }

type existing struct {
	row     store.ActionRow
	payload store.WorklogPayload
}

// Draft proposes st's worklog writes (R6, R7, R8). Corrections come first and ignore
// the work_since floor; the add covers unlogged messages at or after it. A pending
// draft with the same idempotency key is left alone, a different one is superseded,
// and a rejected or failed one with the same key blocks the draft until the set
// changes, retiring any pending draft over a different set. An unpriced model fails the whole ticket with no drafts.
func (s *Sink) Draft(dc attribution.DraftContext, st attribution.TicketState) ([]attribution.Draft, error) {
	var worklogs []existing
	for _, a := range st.Actions {
		if a.Type != store.ActionTypeWorklog {
			continue
		}
		var p store.WorklogPayload
		if err := json.Unmarshal([]byte(a.Payload), &p); err != nil {
			return nil, fmt.Errorf("%s: worklog action %d payload: %w", st.IssueKey, a.ID, err)
		}
		worklogs = append(worklogs, existing{row: a, payload: p})
	}

	var drafts []attribution.Draft
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(st.Moved)) {
		remaining := sortedMessages(st.Logged[id])
		op := store.WorklogOpUpdate
		if len(remaining) == 0 {
			op = store.WorklogOpDelete
		}
		same := func(p store.WorklogPayload) bool {
			return (p.Op == store.WorklogOpUpdate || p.Op == store.WorklogOpDelete) && p.WorklogID == id
		}
		rationale := fmt.Sprintf("correction: %d message(s) logged here are now attributed elsewhere", len(st.Moved[id]))
		d, err := s.resolve(st.IssueKey, op, id, remaining, rationale, filter(worklogs, same))
		if err != nil {
			if d == nil {
				return nil, err
			}
			errs = append(errs, err)
		}
		drafts = append(drafts, d...)
	}

	floor := dc.EnabledAt
	if !s.cfg.WorkSince.IsZero() {
		floor = s.cfg.WorkSince.Time()
	}
	var delta []store.MessageAttribution
	for _, m := range st.Unlogged {
		if !m.OccurredAt.Before(floor) {
			delta = append(delta, m)
		}
	}
	delta = sortedMessages(delta)
	var rationale string
	if len(delta) > 0 {
		rationale = fmt.Sprintf("computed from the token ledger: %d message(s), %s to %s", len(delta),
			delta[0].OccurredAt.UTC().Format(time.RFC3339), delta[len(delta)-1].OccurredAt.UTC().Format(time.RFC3339))
	}
	isAdd := func(p store.WorklogPayload) bool { return p.Op == store.WorklogOpAdd }
	d, err := s.resolve(st.IssueKey, store.WorklogOpAdd, "", delta, rationale, filter(worklogs, isAdd))
	if err != nil {
		if d == nil {
			return nil, err
		}
		errs = append(errs, err)
	}
	drafts = append(drafts, d...)

	return drafts, errors.Join(errs...)
}

// resolve drafts one write against the prior actions of its group (one add per
// ticket, one correction per worklog id). An error with a nil result means no draft
// may be persisted for the ticket; with a result, the draft stands and the error
// asks for a reviewer.
func (s *Sink) resolve(issue, op, worklogID string, msgs []store.MessageAttribution, rationale string,
	prior []existing,
) ([]attribution.Draft, error) {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.MessageID
	}
	key := idempotencyKey(op, issue, worklogID, ids)

	var pending []existing
	blocked := false
	for _, e := range prior {
		switch e.row.Status {
		case store.StatusRejected, store.StatusFailed:
			blocked = blocked || e.payload.IdempotencyKey == key
		case store.StatusProposed, store.StatusApproved:
			pending = append(pending, e)
		}
	}
	slices.SortFunc(pending, func(a, b existing) int { return cmp.Compare(b.row.ID, a.row.ID) })

	var supersedes int64
	var errs []error
	if len(pending) > 0 {
		newest := pending[0]
		if len(pending) > 1 {
			older := make([]string, 0, len(pending)-1)
			for _, e := range pending[1:] {
				older = append(older, fmt.Sprintf("action %d", e.row.ID))
			}
			errs = append(errs, fmt.Errorf("%s: %d pending worklog %s drafts; superseding the newest, action %d; resolve %s by hand",
				issue, len(pending), op, newest.row.ID, strings.Join(older, ", ")))
		}
		if newest.payload.IdempotencyKey == key {
			return []attribution.Draft{}, errors.Join(errs...)
		}
		supersedes = newest.row.ID
	}

	// A reviewer already ruled on this exact set: draft nothing, but retire a pending
	// draft over a different set, which no longer matches what this ticket holds.
	if blocked {
		if supersedes == 0 {
			return nil, nil
		}
		return []attribution.Draft{{Supersedes: supersedes}}, errors.Join(errs...)
	}

	if op == store.WorklogOpAdd && len(msgs) == 0 {
		if supersedes == 0 {
			return nil, nil
		}
		return []attribution.Draft{{Supersedes: supersedes}}, errors.Join(errs...)
	}

	row, err := s.row(issue, op, worklogID, key, ids, msgs, rationale)
	if err != nil {
		return nil, err
	}
	return []attribution.Draft{{Row: row, Supersedes: supersedes}}, errors.Join(errs...)
}

// row builds the proposed action. A delete carries no messages and no time.
func (s *Sink) row(issue, op, worklogID, key string, ids []string, msgs []store.MessageAttribution,
	rationale string,
) (*store.ActionRow, error) {
	p := store.WorklogPayload{Op: op, WorklogID: worklogID, MessageIDs: ids, IdempotencyKey: key}
	if op != store.WorklogOpDelete {
		totals, err := attribution.Totals(msgs, s.pricing)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", issue, err)
		}
		prop, err := property(key, ids, totals)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", issue, err)
		}
		idle := time.Duration(s.cfg.IdleGap)
		if idle == 0 {
			idle = DefaultIdleGap
		}
		p.Started = started(msgs)
		p.TimeSpentSeconds = worklogSeconds(msgs, idle)
		p.Comment = comment(totals)
		p.Property = json.RawMessage(prop)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("%s: marshal worklog payload: %w", issue, err)
	}
	return &store.ActionRow{
		Type: store.ActionTypeWorklog, IssueKey: issue, Payload: string(b),
		Confidence: 1, Rationale: rationale, Status: store.StatusProposed,
	}, nil
}

func filter(all []existing, keep func(store.WorklogPayload) bool) []existing {
	var out []existing
	for _, e := range all {
		if keep(e.payload) {
			out = append(out, e)
		}
	}
	return out
}

// sortedMessages orders msgs by time, then message id, so ids and the key are stable.
func sortedMessages(msgs []store.MessageAttribution) []store.MessageAttribution {
	out := slices.Clone(msgs)
	slices.SortFunc(out, func(a, b store.MessageAttribution) int {
		if c := a.OccurredAt.Compare(b.OccurredAt); c != 0 {
			return c
		}
		return strings.Compare(a.MessageID, b.MessageID)
	})
	return out
}
