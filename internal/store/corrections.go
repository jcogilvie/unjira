package store

// corrections.go reads the reviewer rulings slice 7 distils into rules.
//
// actions.feedback has been written since triage landed and read by nothing — its schema
// comment says so: "Written by slice 6's triage/rework loop, nothing today." This is its
// first consumer, and the query's shape IS the design, because which rows count as a
// correction decides what unjira can learn.

import (
	"encoding/json"
	"fmt"
	"time"
)

// Correction is one reviewer ruling that carried reasoning.
//
// Mirrors rules.Correction field-for-field but is declared here, because this package
// must not import internal/rules — the same reasoning narrativeissues.go gives for
// keeping Provenance a plain string rather than importing the correlator's typed one. The
// caller maps between them.
type Correction struct {
	ActionID   int64
	ActionType string
	IssueKey   string
	// Body is the prose the reviewer was reacting to.
	Body string
	// Feedback is what the reviewer wrote.
	Feedback string
}

// CorrectionsSince returns reviewer corrections newer than the watermark, oldest first.
//
// A zero `since` means everything, which is a first learn-check.
//
// THREE FILTERS, each load-bearing:
//
//   - Status is rejected or edited ONLY. Deliberately not suppressed or declined: the
//     actions schema comment already distinguishes them — suppressed is a deterministic
//     filter's refusal, declined is the MODEL's judgment that work is not worth a ticket.
//     Neither is a human teaching anything, and distilling them would turn unjira's own
//     filters into agent norms, which is the self-reference loop design-notes #11 warns
//     about in a new costume.
//   - Feedback must be non-empty after trimming. Reject takes OPTIONAL text (see
//     cmd/unjira/triage.go's parseDecision: "a reviewer may simply not want the action"),
//     and a refusal with no reasoning teaches nothing a model can generalise. Including
//     it would dilute the corpus with rows whose only content is "no".
//   - decided_at, not created_at, is the watermark column. A correction's age is when the
//     REVIEWER ruled, not when the reconciler drafted the thing they ruled on — those can
//     be days apart, and using created_at would re-offer old drafts a reviewer just judged.
//
// Ordered oldest-first so a distillation prompt reads chronologically, which is how a
// reviewer's thinking developed.
func (s *Store) CorrectionsSince(since time.Time) ([]Correction, error) {
	rows, err := s.db.Query(
		`SELECT a.id, a.type, COALESCE(a.issue_key, ''), a.payload, a.feedback
		   FROM actions a
		  WHERE a.status IN (?, ?)
		    AND a.feedback IS NOT NULL
		    AND TRIM(a.feedback) != ''
		    AND (? = '' OR a.decided_at > ?)
		  ORDER BY a.decided_at, a.id`,
		StatusRejected, StatusEdited, watermark(since), watermark(since),
	)
	if err != nil {
		return nil, fmt.Errorf("querying corrections since %s: %w", watermark(since), err)
	}
	defer func() { _ = rows.Close() }()

	var out []Correction
	for rows.Next() {
		var (
			c       Correction
			payload string
		)
		if err := rows.Scan(&c.ActionID, &c.ActionType, &c.IssueKey, &payload, &c.Feedback); err != nil {
			return nil, fmt.Errorf("scanning correction row: %w", err)
		}

		// The drafted prose, so a correction is legible against what it corrected.
		// "name the affected resource types" is a norm about specificity when read next
		// to a comment that said "the audit", and unintelligible alone.
		c.Body = bodyOfPayload(payload)
		out = append(out, c)
	}

	return out, rows.Err()
}

// watermark formats since for lexical comparison against decided_at, or "" for a zero
// time meaning "everything".
//
// Whole seconds, matching decided_at's own format. Unlike narrative_events.linked_at this
// needs no sub-second precision: a learn-interval is minutes or hours, so two corrections
// in the same second are both either inside or outside any realistic window.
func watermark(since time.Time) string {
	if since.IsZero() {
		return ""
	}

	return since.UTC().Format("2006-01-02T15:04:05Z")
}

// bodyOfPayload extracts an action's human-readable text from its JSON payload.
//
// Mirrors cmd/unjira's bodyOf, deliberately duplicated rather than shared: that one
// exists for terminal DISPLAY and returns the raw payload when decoding fails, because a
// reviewer needs to see that a payload is malformed. This one feeds a distillation prompt,
// where the same fallback is also right — a correction about unreadable prose is still a
// correction — but the two consumers could reasonably diverge, and store must not import
// cmd.
//
// A transition has neither body nor summary (its payload is target_status), so it falls
// through to the raw payload, which reads as {"target_status":"Done"}. That is the honest
// rendering: the "prose" a reviewer rejected on a transition IS the target status.
func bodyOfPayload(payload string) string {
	var p struct {
		Body    string `json:"body"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return payload
	}
	if p.Body != "" {
		return p.Body
	}
	if p.Summary != "" {
		return p.Summary
	}

	return payload
}
