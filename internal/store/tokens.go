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
