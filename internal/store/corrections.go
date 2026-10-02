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
	"slices"
	"strings"
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

// CorrectionsCursor is the learn pass's position among reviewer rulings: the high-water
// mark of what a draft actually READ (finding F31).
//
// WHY NOT A CLOCK READING. The watermark used to be time.Now() at keep, at whole seconds,
// compared lexically against decided_at's milliseconds. That lost corrections two ways. A
// ruling later in the watermark's own second sorted BELOW it, because '.' sorts before
// 'Z'. And a ruling made while the draft was with the model, after the read and before the
// keep, fell below a watermark that recorded when keep ran rather than what was drafted.
// Design-notes #40 is the same family: a comparison against a clock reading decides
// everything inside one tick in one direction.
//
// WHY NOT actions.id. It is monotonic, but it orders CREATION, not ruling. A reviewer rules
// in whatever order they like, so an action drafted early and rejected late has a lower id
// than one already distilled, and an id cursor would skip it forever.
//
// SO: the newest decided_at the draft read, plus the ids of every ruling it read at exactly
// that decided_at. A row is past the cursor if it was decided later, or at the same instant
// and was not one of those read. That cannot tie: two rulings in one millisecond are told
// apart by id, so a ruling committed in the same millisecond as the newest one read, but
// after the read, is still offered. Both sides of every comparison are values of the same
// column in its fixed-width %f format, so lexical order is chronological. No schema change,
// because the store has no migrations.
//
// What it still trusts is that decided_at does not run backwards between two rulings, that
// is, that the wall clock does not step back. A sequence stamped at ruling time would not
// need that, but it needs a new column and a fresh store; see F41.
type CorrectionsCursor struct {
	// DecidedAt is the exact decided_at text of the newest ruling read. Empty means nothing
	// has been read yet, i.e. "everything".
	DecidedAt string `json:"decided_at"`
	// IDs are the actions read whose decided_at equals DecidedAt, ascending. Empty with a
	// non-empty DecidedAt is legal: it means "everything decided at or after DecidedAt",
	// which is the form a hand-converted pre-F31 watermark takes.
	IDs []int64 `json:"ids"`
}

// decidedAtLayout is decided_at's own format, strftime('%Y-%m-%dT%H:%M:%fZ'), in Go's
// notation. A cursor whose DecidedAt is not in it would compare unlike-for-like, which is
// exactly the F31 inversion, so parsing rejects one.
const decidedAtLayout = "2006-01-02T15:04:05.000Z"

// IsZero reports whether the cursor has read nothing, which means "everything".
func (c CorrectionsCursor) IsZero() bool { return c.DecidedAt == "" }

// Encode is the cursor's stored form. JSON, so it cannot be mistaken for the whole-second
// RFC3339 timestamp the pre-F31 watermark stored. The zero cursor is "", which is what
// GetCursor returns for an absent row.
func (c CorrectionsCursor) Encode() (string, error) {
	if c.IsZero() {
		return "", nil
	}

	ids := c.IDs
	if ids == nil {
		ids = []int64{}
	}

	out, err := json.Marshal(CorrectionsCursor{DecidedAt: c.DecidedAt, IDs: ids})
	if err != nil {
		return "", fmt.Errorf("encoding corrections cursor at %s: %w", c.DecidedAt, err)
	}

	return string(out), nil
}

// String is Encode for messages and logs.
func (c CorrectionsCursor) String() string {
	out, err := c.Encode()
	if err != nil {
		return err.Error()
	}

	return out
}

// ParseCorrectionsCursor reads a cursor's stored form. "" is the zero cursor.
//
// Strict: unknown fields, trailing data, a decided_at not in decided_at's own millisecond
// format, or a non-positive id is an error rather than a best guess. A misread cursor
// silently skips or re-offers corrections, and neither shows in the output.
func ParseCorrectionsCursor(raw string) (CorrectionsCursor, error) {
	if raw == "" {
		return CorrectionsCursor{}, nil
	}

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()

	var c CorrectionsCursor
	if err := dec.Decode(&c); err != nil {
		return CorrectionsCursor{}, fmt.Errorf("corrections cursor %q is not cursor JSON: %w", raw, err)
	}
	if dec.More() {
		return CorrectionsCursor{}, fmt.Errorf("corrections cursor %q has trailing data", raw)
	}
	if _, err := time.Parse(decidedAtLayout, c.DecidedAt); err != nil {
		return CorrectionsCursor{}, fmt.Errorf(
			"corrections cursor %q: decided_at must be in decided_at's own format %s: %w",
			raw, decidedAtLayout, err)
	}
	for _, id := range c.IDs {
		if id <= 0 {
			return CorrectionsCursor{}, fmt.Errorf(
				"corrections cursor %q: %d is not an action id", raw, id)
		}
	}

	return c, nil
}

// Later returns whichever of c and other has read further, so a watermark set from it never
// moves backwards. At the same DecidedAt the id sets are unioned: each lists rulings read at
// that instant, and both were read.
func (c CorrectionsCursor) Later(other CorrectionsCursor) CorrectionsCursor {
	switch {
	case other.DecidedAt > c.DecidedAt:
		return other
	case other.DecidedAt < c.DecidedAt:
		return c
	}

	ids := slices.Concat(c.IDs, other.IDs)
	slices.Sort(ids)

	return CorrectionsCursor{DecidedAt: c.DecidedAt, IDs: slices.Compact(ids)}
}

// CorrectionsSince returns reviewer corrections past the cursor, oldest first, together with
// the cursor advanced through every one of them: the high-water mark of what this read saw.
// A caller that keeps rules drafted from these corrections records THAT cursor, not a clock
// reading, so a ruling made after this read is still past it.
//
// A zero `since` means everything, which is a first learn-check. When nothing is returned,
// the returned cursor is `since` unchanged.
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
//   - decided_at, not created_at, is the cursor column. A correction's age is when the
//     REVIEWER ruled, not when the reconciler drafted the thing they ruled on — those can
//     be days apart, and using created_at would re-offer old drafts a reviewer just judged.
//
// Ordered oldest-first so a distillation prompt reads chronologically, which is how a
// reviewer's thinking developed.
func (s *Store) CorrectionsSince(since CorrectionsCursor) ([]Correction, CorrectionsCursor, error) {
	readAtSince := since.IDs
	if readAtSince == nil {
		readAtSince = []int64{}
	}
	readAtSinceJSON, err := json.Marshal(readAtSince)
	if err != nil {
		return nil, since, fmt.Errorf("encoding the ids read at %s: %w", since.DecidedAt, err)
	}

	// decided_at IS NOT NULL is implied by the status filter today (only the status-update
	// path writes rejected/edited, and it stamps decided_at), but a NULL would make every
	// comparison below NULL and drop the row from a non-zero cursor's read without a word.
	// Stated, so a future direct insert in a ruled status cannot hide that way.
	rows, err := s.db.Query(
		`SELECT a.id, a.type, COALESCE(a.issue_key, ''), a.payload, a.feedback, a.decided_at
		   FROM actions a
		  WHERE a.status IN (?, ?)
		    AND a.feedback IS NOT NULL
		    AND TRIM(a.feedback) != ''
		    AND a.decided_at IS NOT NULL
		    AND (? = ''
		         OR a.decided_at > ?
		         OR (a.decided_at = ? AND a.id NOT IN (SELECT value FROM json_each(?))))
		  ORDER BY a.decided_at, a.id`,
		StatusRejected, StatusEdited,
		since.DecidedAt, since.DecidedAt, since.DecidedAt, string(readAtSinceJSON),
	)
	if err != nil {
		return nil, since, fmt.Errorf("querying corrections since %s: %w", since, err)
	}
	defer func() { _ = rows.Close() }()

	var (
		out    []Correction
		cursor = since
	)
	for rows.Next() {
		var (
			c         Correction
			payload   string
			decidedAt string
		)
		if err := rows.Scan(&c.ActionID, &c.ActionType, &c.IssueKey, &payload, &c.Feedback,
			&decidedAt); err != nil {
			return nil, since, fmt.Errorf("scanning correction row: %w", err)
		}

		// The drafted prose, so a correction is legible against what it corrected.
		// "name the affected resource types" is a norm about specificity when read next
		// to a comment that said "the audit", and unintelligible alone.
		c.Body = bodyOfPayload(payload)
		out = append(out, c)
		cursor = cursor.Later(CorrectionsCursor{DecidedAt: decidedAt, IDs: []int64{c.ActionID}})
	}
	if err := rows.Err(); err != nil {
		return nil, since, fmt.Errorf("reading correction rows: %w", err)
	}

	return out, cursor, nil
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
