package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
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
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (collector, resource)
		 DO UPDATE SET position = excluded.position, updated_at = excluded.updated_at`,
		collector, resource, position, s.now(),
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
	Latest    time.Time
}

// CursorCounts returns the number of tracked resources and the latest
// updated_at, grouped by collector. The latest is read from its row by a scalar
// subquery, as EventCountsBySource reads its own, so it stays a DATETIME.
func (s *Store) CursorCounts() ([]CollectorCount, error) {
	rows, err := s.db.Query(
		`SELECT c.collector, COUNT(*) AS n,
		        (SELECT l.updated_at FROM cursors l WHERE l.collector = c.collector
		          ORDER BY l.updated_at DESC LIMIT 1) AS latest
		   FROM cursors c GROUP BY c.collector`,
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
