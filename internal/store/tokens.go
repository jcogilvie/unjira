package store

import (
	"database/sql"
	"fmt"
	"time"
)

// -- token ledger --------------------------------------------------------------

// MessageUsage is one token_messages row: what one Claude Code assistant message.id
// cost, as read from one transcript. AgentID and CoveringExternalID are NULL in the
// table when empty here.
type MessageUsage struct {
	MessageID          string
	TranscriptID       string
	AgentID            string
	OccurredAt         time.Time
	Model              string
	InputTokens        int64
	OutputTokens       int64
	CacheWriteTokens   int64
	CacheReadTokens    int64
	CoveringExternalID string
}

// upsertMessageUsageSQL enforces token_messages' ownership rule (see its schema
// comment). The four counters always rise to the MAX seen. The holder is the
// transcript with the smallest (occurred_at, transcript_id), so the owner is the same
// whatever order transcripts are read in, in one pass or across passes: a different
// transcript takes the row, with every non-counter column of its own, only when it
// sorts before the current holder. Within the holder's own transcript the earliest
// occurred_at wins and a NULL covering id never erases a set one. In DO UPDATE every
// unqualified column is the row's value before the update, so the CASEs all compare
// against the old holder.
const upsertMessageUsageSQL = `
INSERT INTO token_messages (
    message_id, transcript_id, agent_id, occurred_at, model,
    input_tokens, output_tokens, cache_write_tokens, cache_read_tokens,
    covering_external_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (message_id) DO UPDATE SET
    input_tokens       = MAX(input_tokens, excluded.input_tokens),
    output_tokens      = MAX(output_tokens, excluded.output_tokens),
    cache_write_tokens = MAX(cache_write_tokens, excluded.cache_write_tokens),
    cache_read_tokens  = MAX(cache_read_tokens, excluded.cache_read_tokens),
    -- A transcript that does not take the row has an occurred_at no earlier than the
    -- holder's, so MIN is right in every case.
    occurred_at        = MIN(occurred_at, excluded.occurred_at),
    transcript_id      = CASE WHEN ` + otherTranscriptTakes + `
                              THEN excluded.transcript_id ELSE transcript_id END,
    agent_id           = CASE WHEN ` + otherTranscriptTakes + `
                              THEN excluded.agent_id ELSE agent_id END,
    model              = CASE WHEN ` + otherTranscriptTakes + `
                              THEN excluded.model ELSE model END,
    covering_external_id = CASE
        WHEN ` + otherTranscriptTakes + ` THEN excluded.covering_external_id
        WHEN excluded.transcript_id = transcript_id
            THEN COALESCE(excluded.covering_external_id, covering_external_id)
        ELSE covering_external_id END`

const otherTranscriptTakes = `excluded.transcript_id <> transcript_id
      AND (excluded.occurred_at, excluded.transcript_id) < (occurred_at, transcript_id)`

// UpsertMessageUsage writes rows in one transaction, merging each into any row already
// held for its message id by the rule on upsertMessageUsageSQL.
func (s *Store) UpsertMessageUsage(rows []MessageUsage) error {
	return s.WithTx(func(tx *Tx) error {
		stmt, err := tx.tx.Prepare(upsertMessageUsageSQL)
		if err != nil {
			return fmt.Errorf("preparing token_messages upsert: %w", err)
		}
		defer func() { _ = stmt.Close() }()

		for _, r := range rows {
			if _, err := stmt.Exec(
				r.MessageID, r.TranscriptID, nullIfEmpty(r.AgentID), r.OccurredAt, r.Model,
				r.InputTokens, r.OutputTokens, r.CacheWriteTokens, r.CacheReadTokens,
				nullIfEmpty(r.CoveringExternalID),
			); err != nil {
				return fmt.Errorf("upserting token_messages %s: %w", r.MessageID, err)
			}
		}

		return nil
	})
}

func nullIfEmpty(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// AttributionState is where a ledgered message's tokens are charged, as
// MessageAttributions computes it.
type AttributionState string

// The four attribution states.
const (
	// AttributionTracked: the covering event's member narrative has a confident primary
	// (confidentPrimary), and the message is charged to that issue.
	AttributionTracked AttributionState = "tracked"
	// AttributionUntracked: the member narrative has no confident primary, so its work is
	// untracked, the same set the create path proposes tickets for. IssueKey is the
	// below-floor primary as a candidate, or empty when there is no primary at all.
	AttributionUntracked AttributionState = "untracked"
	// AttributionUnattributed: the covering event exists but has no member link. A context
	// link does not count: context is some other narrative's work, never charged here.
	AttributionUnattributed AttributionState = "unattributed"
	// AttributionNoEvent: the row names no covering event, or one never stored.
	AttributionNoEvent AttributionState = "no_event"
)

// MessageAttribution is one ledgered message and where its tokens are charged now.
// NarrativeID is 0 unless State is tracked or untracked; IssueKey and Connection are
// empty unless the member narrative has a primary.
type MessageAttribution struct {
	MessageUsage

	IssueKey    string
	Connection  string
	NarrativeID int64
	State       AttributionState

	// WorklogID and WorklogIssueKey name the Jira worklog the message is logged in, or
	// are empty while it is unlogged.
	WorklogID       string
	WorklogIssueKey string
}

// messageAttributionsSQL joins message -> covering event -> member link -> primary, in
// that order, at read time; nothing is cached, so Tx.MoveMember, a primary relink and a
// triage merge or split show on the next read. Every join is at most one row, so each
// message yields exactly one: events is UNIQUE (source, external_id), and
// one_member_link_per_event and one_primary_per_narrative do the rest. The kind filter
// is mandatory: a context link must never reach attribution. The source is the
// claude_code collector's name (claudecode.Name, which this package cannot import),
// the only producer of covering ids.
//
// The ledger is read whole, so token_messages is scanned (in message-id order, which
// also serves the ORDER BY) and each join is a unique-index lookup. EXPLAIN QUERY PLAN
// on modernc.org/sqlite, 2026-10-14:
//
//	SCAN tm USING INDEX sqlite_autoindex_token_messages_1
//	SEARCH e USING COVERING INDEX sqlite_autoindex_events_1 (source=? AND external_id=?) LEFT-JOIN
//	SEARCH ne USING INDEX one_member_link_per_event (event_id=?) LEFT-JOIN
//	SEARCH ni USING INDEX one_primary_per_narrative (narrative_id=?) LEFT-JOIN
const messageAttributionsSQL = `
SELECT tm.message_id, tm.transcript_id, COALESCE(tm.agent_id, ''), tm.occurred_at, tm.model,
       tm.input_tokens, tm.output_tokens, tm.cache_write_tokens, tm.cache_read_tokens,
       COALESCE(tm.covering_external_id, ''),
       COALESCE(ni.issue_key, ''), COALESCE(ni.connection, ''), COALESCE(ne.narrative_id, 0),
       CASE
           WHEN e.id IS NULL THEN '` + string(AttributionNoEvent) + `'
           WHEN ne.narrative_id IS NULL THEN '` + string(AttributionUnattributed) + `'
           WHEN ` + confidentPrimary + ` THEN '` + string(AttributionTracked) + `'
           ELSE '` + string(AttributionUntracked) + `'
       END,
       COALESCE(tm.worklog_id, ''), COALESCE(tm.worklog_issue_key, '')
FROM token_messages tm
LEFT JOIN events e
       ON e.source = 'claude_code' AND e.external_id = tm.covering_external_id
LEFT JOIN narrative_events ne
       ON ne.event_id = e.id AND ne.kind = '` + string(LinkMember) + `'
LEFT JOIN narrative_issues ni
       ON ni.narrative_id = ne.narrative_id AND ni.role = '` + string(RolePrimary) + `'
ORDER BY tm.message_id`

// MessageAttributions returns every ledgered message with its attribution as of now,
// ordered by message id. confidenceFloor is match.confidence_floor: a primary below it
// leaves its narrative untracked, as it does for the create path (confidentPrimary).
func (s *Store) MessageAttributions(confidenceFloor float64) ([]MessageAttribution, error) {
	rows, err := s.db.Query(messageAttributionsSQL, confidenceFloor)
	if err != nil {
		return nil, fmt.Errorf("querying token attributions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []MessageAttribution
	for rows.Next() {
		var a MessageAttribution
		if err := rows.Scan(&a.MessageID, &a.TranscriptID, &a.AgentID, &a.OccurredAt, &a.Model,
			&a.InputTokens, &a.OutputTokens, &a.CacheWriteTokens, &a.CacheReadTokens,
			&a.CoveringExternalID, &a.IssueKey, &a.Connection, &a.NarrativeID, &a.State,
			&a.WorklogID, &a.WorklogIssueKey); err != nil {
			return nil, fmt.Errorf("scanning token attribution: %w", err)
		}
		out = append(out, a)
	}

	return out, rows.Err()
}

// LLMCall is one llm_calls row: one call unjira itself made to its LLM. CachedTokens
// is nil when the server reported none.
type LLMCall struct {
	CalledAt         time.Time
	Stage            string
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     *int64
}

// RecordLLMCall inserts one llm_calls row. called_at is c.CalledAt exactly: the store
// never reads the clock for it.
func (s *Store) RecordLLMCall(c LLMCall) error {
	_, err := s.db.Exec(
		`INSERT INTO llm_calls (called_at, stage, model, prompt_tokens, completion_tokens, cached_tokens)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.CalledAt, c.Stage, c.Model, c.PromptTokens, c.CompletionTokens, c.CachedTokens,
	)
	if err != nil {
		return fmt.Errorf("recording llm call (stage %s): %w", c.Stage, err)
	}

	return nil
}

// LLMCallsSince returns every llm_calls row called at or after since, in insertion order.
func (s *Store) LLMCallsSince(since time.Time) ([]LLMCall, error) {
	rows, err := s.db.Query(
		`SELECT called_at, stage, model, prompt_tokens, completion_tokens, cached_tokens
		 FROM llm_calls WHERE called_at >= ? ORDER BY id`, since)
	if err != nil {
		return nil, fmt.Errorf("querying llm calls since %s: %w", since.Format(time.RFC3339), err)
	}
	defer func() { _ = rows.Close() }()

	var out []LLMCall
	for rows.Next() {
		var c LLMCall
		if err := rows.Scan(&c.CalledAt, &c.Stage, &c.Model, &c.PromptTokens,
			&c.CompletionTokens, &c.CachedTokens); err != nil {
			return nil, fmt.Errorf("scanning llm call: %w", err)
		}
		out = append(out, c)
	}

	return out, rows.Err()
}
