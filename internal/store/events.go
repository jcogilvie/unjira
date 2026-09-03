package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jcogilvie/unjira/internal/events"
)

// ErrEventNotFound is returned by EventIDByExternalID when no event matches
// the given (source, external_id).
var ErrEventNotFound = errors.New("event not found")

// -- events ------------------------------------------------------------

// InsertEvent inserts an event; returns false if it was already present.
func (s *Store) InsertEvent(event events.Event) (bool, error) {
	artifacts, err := json.Marshal(event.Artifacts)
	if err != nil {
		return false, fmt.Errorf("marshaling artifacts for %s: %w", event.ExternalID, err)
	}

	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO events (source, external_id, occurred_at, actor, summary, artifacts, raw_ref)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		event.Source, event.ExternalID, event.OccurredAt.Format(time.RFC3339),
		nullable(event.Actor), event.Summary, string(artifacts), nullable(event.RawRef),
	)
	if err != nil {
		return false, fmt.Errorf("inserting event %s/%s: %w", event.Source, event.ExternalID, err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking rows affected for %s/%s: %w", event.Source, event.ExternalID, err)
	}

	return rows > 0, nil
}

// EventsOn returns every event that occurred on the given day (UTC).
func (s *Store) EventsOn(day time.Time) ([]events.Event, error) {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)

	rows, err := s.db.Query(
		`SELECT source, external_id, occurred_at, actor, summary, artifacts, raw_ref
		 FROM events WHERE occurred_at >= ? AND occurred_at < ? ORDER BY occurred_at`,
		start.Format(time.RFC3339), end.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("querying events on %s: %w", day.Format("2006-01-02"), err)
	}
	defer func() { _ = rows.Close() }()

	var out []events.Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning event row: %w", err)
		}
		out = append(out, event)
	}

	return out, rows.Err()
}

// SourceCount is one row of EventCountsBySource.
type SourceCount struct {
	Source string
	Count  int
	Latest string
}

// EventCountsBySource returns the number of events and the latest
// occurred_at, grouped by source.
func (s *Store) EventCountsBySource() ([]SourceCount, error) {
	rows, err := s.db.Query(
		`SELECT source, COUNT(*) AS n, MAX(occurred_at) AS latest FROM events GROUP BY source`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying event counts by source: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SourceCount
	for rows.Next() {
		var sc SourceCount
		if err := rows.Scan(&sc.Source, &sc.Count, &sc.Latest); err != nil {
			return nil, fmt.Errorf("scanning source count row: %w", err)
		}
		out = append(out, sc)
	}

	return out, rows.Err()
}

// EventIDByExternalID returns the row id of the event with the given
// (source, external_id), or ErrEventNotFound.
func (s *Store) EventIDByExternalID(source, externalID string) (int64, error) {
	return eventIDByExternalIDImpl(s.db, source, externalID)
}

// EventIDByExternalID is the *Tx-scoped variant of
// (*Store).EventIDByExternalID.
func (t *Tx) EventIDByExternalID(source, externalID string) (int64, error) {
	return eventIDByExternalIDImpl(t.tx, source, externalID)
}

func eventIDByExternalIDImpl(c dbConn, source, externalID string) (int64, error) {
	var id int64
	err := c.QueryRow(
		`SELECT id FROM events WHERE source = ? AND external_id = ?`, source, externalID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrEventNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("querying event id for %s/%s: %w", source, externalID, err)
	}

	return id, nil
}

// scanRow is the subset of *sql.Rows this package needs to scan an event.
type scanRow interface {
	Scan(dest ...any) error
}

func scanEvent(row scanRow) (events.Event, error) {
	var (
		e          events.Event
		occurredAt string
		actor      sql.NullString
		artifacts  string
		rawRef     sql.NullString
	)

	if err := row.Scan(&e.Source, &e.ExternalID, &occurredAt, &actor, &e.Summary, &artifacts, &rawRef); err != nil {
		return events.Event{}, err
	}

	parsed, err := time.Parse(time.RFC3339, occurredAt)
	if err != nil {
		return events.Event{}, fmt.Errorf("parsing occurred_at %q: %w", occurredAt, err)
	}
	e.OccurredAt = parsed

	e.Actor = actor.String
	e.RawRef = rawRef.String

	e.Artifacts = make(map[string]any)
	if err := json.Unmarshal([]byte(artifacts), &e.Artifacts); err != nil {
		return events.Event{}, fmt.Errorf("unmarshaling artifacts %q: %w", artifacts, err)
	}

	return e, nil
}

// nullable converts an empty string to a SQL NULL so optional Event fields
// (Actor, RawRef) round-trip the same way the Python implementation stored
// them, instead of persisting an empty string. Shared beyond this file: every
// accessor across this package that writes an optional TEXT column
// (narratives, actions, narrative_issues, local issues) uses it too.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
