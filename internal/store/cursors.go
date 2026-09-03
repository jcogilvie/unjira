package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// -- cursors -----------------------------------------------------------

// GetCursor returns the stored position for (collector, resource), or an
// empty string if none is stored.
func (s *Store) GetCursor(collector, resource string) (string, error) {
	var position string

	err := s.db.QueryRow(
		`SELECT position FROM cursors WHERE collector = ? AND resource = ?`,
		collector, resource,
	).Scan(&position)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("getting cursor %s/%s: %w", collector, resource, err)
	}

	return position, nil
}

// SetCursor upserts the position for (collector, resource).
func (s *Store) SetCursor(collector, resource, position string) error {
	_, err := s.db.Exec(
		`INSERT INTO cursors (collector, resource, position, updated_at)
		 VALUES (?, ?, ?, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
		 ON CONFLICT (collector, resource)
		 DO UPDATE SET position = excluded.position, updated_at = excluded.updated_at`,
		collector, resource, position,
	)
	if err != nil {
		return fmt.Errorf("setting cursor %s/%s: %w", collector, resource, err)
	}

	return nil
}

// CollectorCount is one row of CursorCounts.
type CollectorCount struct {
	Collector string
	Count     int
	Latest    string
}

// CursorCounts returns the number of tracked resources and the latest
// updated_at, grouped by collector.
func (s *Store) CursorCounts() ([]CollectorCount, error) {
	rows, err := s.db.Query(
		`SELECT collector, COUNT(*) AS n, MAX(updated_at) AS latest FROM cursors GROUP BY collector`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying cursor counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CollectorCount
	for rows.Next() {
		var cc CollectorCount
		if err := rows.Scan(&cc.Collector, &cc.Count, &cc.Latest); err != nil {
			return nil, fmt.Errorf("scanning cursor count row: %w", err)
		}
		out = append(out, cc)
	}

	return out, rows.Err()
}
