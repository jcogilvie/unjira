package store

import "fmt"

// StoredIssueKeys returns every issue key the store refers to — linked from a narrative
// or targeted by an action — sorted, each once.
//
// It exists for the startup report of keys that no longer route to a configured
// tracker. Routing is not stored, so a config change can leave a stored key with no
// tracker; such a key is reported, never deleted, which is why this reads both tables
// rather than only the links.
func (s *Store) StoredIssueKeys() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT issue_key FROM narrative_issues
		 UNION
		 SELECT issue_key FROM actions WHERE issue_key IS NOT NULL AND issue_key != ''
		 ORDER BY 1`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing stored issue keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var keys []string

	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scanning stored issue key: %w", err)
		}

		keys = append(keys, key)
	}

	return keys, rows.Err()
}
