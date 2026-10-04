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
	"strconv"
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

// CorrectionsCursor is the learn pass's position among reviewer corrections: the highest
// correction sequence a draft actually READ (actions.corrected_seq, issued by
// correction_marks).
//
// WHY A SEQUENCE. Three earlier positions each lost corrections without a word:
//
//   - A clock reading at keep (pre-F31). A ruling made while the draft was with the
//     model fell below a watermark recording when keep ran, not what was drafted.
//   - The newest decided_at read, plus the ids read at that instant (F31). Correct for
//     one ruling per action, but decided_at keeps the FIRST ruling's time, so a lesson
//     added by a later ruling sat below a cursor already past it (F42). And decided_at
//     is the wall clock, so a clock stepping back hid a ruling (F41).
//   - actions.id was never a candidate: it orders creation, and a reviewer rules in any
//     order.
//
// The sequence is stamped when a row BECOMES a correction, which is the event the learn
// pass consumes, and it comes from an AUTOINCREMENT that never reissues a number. So a
// correction made after a read always has a higher sequence than everything that read
// saw, whatever the clock says and however many times the action was ruled before.
type CorrectionsCursor struct {
	// Seq is the highest correction sequence read. Zero means nothing has been read yet,
	// i.e. "everything".
	Seq int64
}

// IsZero reports whether the cursor has read nothing, which means "everything".
func (c CorrectionsCursor) IsZero() bool { return c.Seq == 0 }

// Encode is the cursor's stored form: the sequence in decimal. The zero cursor is "", which
// is what GetCursor returns for an absent row.
func (c CorrectionsCursor) Encode() string {
	if c.IsZero() {
		return ""
	}

	return strconv.FormatInt(c.Seq, 10)
}

// String is Encode for messages and logs.
func (c CorrectionsCursor) String() string { return c.Encode() }

// ParseCorrectionsCursor reads a cursor's stored form. "" is the zero cursor.
//
// Strict: anything but a positive decimal integer with no sign, space or leading zero is an
// error rather than a best guess. That includes both earlier forms, a whole-second
// timestamp and the F31 JSON, neither of which can say what it covered in sequence terms.
// A misread cursor silently skips or re-offers corrections, and neither shows in the output.
func ParseCorrectionsCursor(raw string) (CorrectionsCursor, error) {
	if raw == "" {
		return CorrectionsCursor{}, nil
	}

	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq <= 0 || strconv.FormatInt(seq, 10) != raw {
		return CorrectionsCursor{}, fmt.Errorf(
			"corrections cursor %q is not a correction sequence (a positive decimal integer)", raw)
	}

	return CorrectionsCursor{Seq: seq}, nil
}

// Later returns whichever of c and other has read further, so a watermark set from it never
// moves backwards.
func (c CorrectionsCursor) Later(other CorrectionsCursor) CorrectionsCursor {
	if other.Seq > c.Seq {
		return other
	}

	return c
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
//   - corrected_seq is the cursor column: when the row BECAME a correction. Not created_at,
//     which is when the reconciler drafted the thing ruled on, days earlier, and would
//     re-offer old drafts a reviewer just judged. Not decided_at either, which is the first
//     ruling's clock time (see CorrectionsCursor for both of its defects).
//
// Ordered by when each became a correction, so a distillation prompt reads in the order
// the reviewer's thinking developed. An action whose lesson was revised appears once, with
// its current feedback, at the position of its latest revision.
func (s *Store) CorrectionsSince(since CorrectionsCursor) ([]Correction, CorrectionsCursor, error) {
	// corrected_seq IS NOT NULL is implied by the status and feedback filters (the schema's
	// CHECK refuses a correction without one), and stated so the query does not depend on a
	// constraint someone could relax.
	rows, err := s.db.Query(
		`SELECT a.id, a.type, COALESCE(a.issue_key, ''), a.payload, a.feedback, a.corrected_seq
		   FROM actions a
		  WHERE a.status IN (?, ?)
		    AND a.feedback IS NOT NULL
		    AND TRIM(a.feedback) != ''
		    AND a.corrected_seq IS NOT NULL
		    AND a.corrected_seq > ?
		  ORDER BY a.corrected_seq`,
		StatusRejected, StatusEdited, since.Seq,
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
			c       Correction
			payload string
			seq     int64
		)
		if err := rows.Scan(&c.ActionID, &c.ActionType, &c.IssueKey, &payload, &c.Feedback,
			&seq); err != nil {
			return nil, since, fmt.Errorf("scanning correction row: %w", err)
		}

		// The drafted prose, so a correction is legible against what it corrected.
		// "name the affected resource types" is a norm about specificity when read next
		// to a comment that said "the audit", and unintelligible alone.
		c.Body = bodyOfPayload(payload)
		out = append(out, c)
		cursor = cursor.Later(CorrectionsCursor{Seq: seq})
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
