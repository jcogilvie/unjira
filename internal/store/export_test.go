package store

// export_test.go exposes raw SQL to this package's external tests (store_test) and to
// nothing else: a _test.go file in package store compiles only into this package's own
// test binary, so these never become API another package could reach.
//
// It exists for the F30 tests, which must write timestamps EXPLICITLY — byte-identical,
// or deliberately inverted — to prove the display timestamps no longer decide anything.
// Relying on the wall clock to produce a same-millisecond collision is exactly the
// one-in-thirty flake those tests replace.

// ExecForTest runs one statement against the store's database.
func (s *Store) ExecForTest(query string, args ...any) error {
	_, err := s.db.Exec(query, args...)

	return err
}

// QueryStringsForTest runs a single-column query and scans every row as a string.
func (s *Store) QueryStringsForTest(query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}

	return out, rows.Err()
}

// QueryStringForTest runs a single-row, single-column query and scans it as a string.
func (s *Store) QueryStringForTest(query string, args ...any) (string, error) {
	var out string
	err := s.db.QueryRow(query, args...).Scan(&out)

	return out, err
}

// MessageUsageRows returns every token_messages row, ordered by message id.
func (s *Store) MessageUsageRows() ([]MessageUsage, error) {
	rows, err := s.db.Query(
		`SELECT message_id, transcript_id, COALESCE(agent_id, ''), occurred_at, model,
		        input_tokens, output_tokens, cache_write_tokens, cache_read_tokens,
		        COALESCE(covering_external_id, '')
		 FROM token_messages ORDER BY message_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []MessageUsage
	for rows.Next() {
		var r MessageUsage
		if err := rows.Scan(&r.MessageID, &r.TranscriptID, &r.AgentID, &r.OccurredAt, &r.Model,
			&r.InputTokens, &r.OutputTokens, &r.CacheWriteTokens, &r.CacheReadTokens,
			&r.CoveringExternalID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}

	return out, rows.Err()
}

// LLMCallRows returns every llm_calls row in insertion order.
func (s *Store) LLMCallRows() ([]LLMCall, error) {
	rows, err := s.db.Query(
		`SELECT called_at, stage, model, prompt_tokens, completion_tokens, cached_tokens
		 FROM llm_calls ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []LLMCall
	for rows.Next() {
		var c LLMCall
		if err := rows.Scan(&c.CalledAt, &c.Stage, &c.Model, &c.PromptTokens,
			&c.CompletionTokens, &c.CachedTokens); err != nil {
			return nil, err
		}
		out = append(out, c)
	}

	return out, rows.Err()
}
