// Package store provides SQLite persistence: events, collector cursors, and
// the phase-1+ tables.
//
// The event log is append-only; (source, external_id) is the dedup key so
// collectors can safely re-emit. Narratives/actions/estimates/ledger are
// created now so the schema is stable, but only events and cursors are
// written in phase 0.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/jcogilvie/unjira/internal/events"
)

// ErrLocalIssueNotFound is returned by the local-issue accessors when no
// row matches the given key — unlike GetCursor's silent-empty-string
// convenience, "not found" is meaningful here and must not be swallowed.
var ErrLocalIssueNotFound = errors.New("local issue not found")

const schema = `
CREATE TABLE IF NOT EXISTS events (
    id           INTEGER PRIMARY KEY,
    source       TEXT NOT NULL,
    external_id  TEXT NOT NULL,
    occurred_at  TEXT NOT NULL,
    actor        TEXT,
    summary      TEXT NOT NULL,
    artifacts    TEXT NOT NULL DEFAULT '{}',
    raw_ref      TEXT,
    ingested_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (source, external_id)
);
CREATE INDEX IF NOT EXISTS idx_events_occurred ON events (occurred_at);

CREATE TABLE IF NOT EXISTS cursors (
    collector  TEXT NOT NULL,
    resource   TEXT NOT NULL,
    position   TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (collector, resource)
);

CREATE TABLE IF NOT EXISTS narratives (
    id           INTEGER PRIMARY KEY,
    window_start TEXT NOT NULL,
    window_end   TEXT NOT NULL,
    title        TEXT NOT NULL,
    summary      TEXT NOT NULL,
    issue_key    TEXT,
    confidence   REAL,
    status       TEXT NOT NULL DEFAULT 'open',
    compaction_boundary TEXT,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS narrative_events (
    narrative_id INTEGER NOT NULL REFERENCES narratives (id),
    event_id     INTEGER NOT NULL REFERENCES events (id),
    PRIMARY KEY (narrative_id, event_id)
);

CREATE TABLE IF NOT EXISTS actions (
    id           INTEGER PRIMARY KEY,
    narrative_id INTEGER REFERENCES narratives (id),
    type         TEXT NOT NULL,       -- comment | transition | create | estimate
    issue_key    TEXT,
    payload      TEXT NOT NULL,       -- JSON, shape depends on type
    confidence   REAL,
    rationale    TEXT,
    status       TEXT NOT NULL DEFAULT 'proposed',
                                      -- proposed | approved | edited | rejected | applied | failed
    decided_at   TEXT,
    executed_at  TEXT,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS estimates (
    id         INTEGER PRIMARY KEY,
    issue_key  TEXT NOT NULL,
    method     TEXT NOT NULL,         -- which framing produced it, or 'ensemble'
    value      REAL NOT NULL,
    spread     REAL,
    actual     REAL,                  -- backfilled after completion, for calibration
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS ledger (
    id              INTEGER PRIMARY KEY,
    occurred_at     TEXT NOT NULL,
    description     TEXT NOT NULL,
    source_event_id INTEGER REFERENCES events (id),
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- The local tasktracker backend's own mimicked issue store — distinct from
-- narratives/actions above, which are unjira's own clustering/proposal
-- records, not a mimicked tracker's issue records.
CREATE TABLE IF NOT EXISTS local_issues (
    key             TEXT PRIMARY KEY,
    project         TEXT NOT NULL,
    summary         TEXT NOT NULL,
    description     TEXT,
    issue_type      TEXT NOT NULL,
    status_category TEXT NOT NULL DEFAULT 'todo',
    labels          TEXT NOT NULL DEFAULT '[]',
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS local_issue_comments (
    id         INTEGER PRIMARY KEY,
    issue_key  TEXT NOT NULL REFERENCES local_issues (key),
    body       TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
`

// Store wraps a SQLite connection with unjira's schema and access methods.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at dbPath, ensures its
// parent directory exists, and applies the schema.
func Open(dbPath string) (*Store, error) {
	if dir := filepath.Dir(dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("creating directory for %s: %w", dbPath, err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", dbPath, err)
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("applying schema to %s: %w", dbPath, err)
	}

	if err := ensureNarrativeCompactionBoundary(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating narratives.compaction_boundary in %s: %w", dbPath, err)
	}

	return &Store{db: db}, nil
}

// ensureNarrativeCompactionBoundary adds narratives.compaction_boundary to a
// database whose narratives table predates the column. CREATE TABLE IF NOT
// EXISTS is a no-op on an existing table, so a fresh DB gets the column from
// the schema while an older DB needs this explicit ALTER. Idempotent: it
// checks column presence first and does nothing when already present.
func ensureNarrativeCompactionBoundary(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(narratives)`)
	if err != nil {
		return fmt.Errorf("reading narratives table info: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid                 int
			name, colType       string
			notNull, primaryKey int
			dfltValue           sql.NullString
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &primaryKey); err != nil {
			return fmt.Errorf("scanning narratives table info: %w", err)
		}
		if name == "compaction_boundary" {
			return rows.Err() // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := db.Exec(`ALTER TABLE narratives ADD COLUMN compaction_boundary TEXT`); err != nil {
		return fmt.Errorf("adding compaction_boundary column: %w", err)
	}

	return nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// NarrativeColumns returns the column names of the narratives table — a thin
// introspection helper for tests verifying the compaction_boundary migration.
func (s *Store) NarrativeColumns() ([]string, error) {
	rows, err := s.db.Query(`PRAGMA table_info(narratives)`)
	if err != nil {
		return nil, fmt.Errorf("reading narratives table info: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var cols []string
	for rows.Next() {
		var (
			cid                 int
			name, colType       string
			notNull, primaryKey int
			dfltValue           sql.NullString
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("scanning narratives table info: %w", err)
		}
		cols = append(cols, name)
	}

	return cols, rows.Err()
}

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

// -- cursors -----------------------------------------------------------

// GetCursor returns the stored position for (collector, resource), or an
// empty string if none is stored.
func (s *Store) GetCursor(collector, resource string) (string, error) {
	var position string

	err := s.db.QueryRow(
		`SELECT position FROM cursors WHERE collector = ? AND resource = ?`,
		collector, resource,
	).Scan(&position)
	if err == sql.ErrNoRows {
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

// -- local issues (the local tasktracker backend's own mimicked store) -----

// LocalIssue is one row of local_issues.
type LocalIssue struct {
	Key            string
	Project        string
	Summary        string
	Description    string
	IssueType      string
	StatusCategory string
	Labels         []string
}

// InsertLocalIssue creates a local issue, assigning it the next sequential
// "PROJECT-N" key for project. Returns the assigned key.
func (s *Store) InsertLocalIssue(project, summary, issueType, description string, labels []string) (string, error) {
	var maxN sql.NullInt64
	if err := s.db.QueryRow(
		`SELECT MAX(CAST(substr(key, length(?) + 2) AS INTEGER)) FROM local_issues WHERE project = ?`,
		project, project,
	).Scan(&maxN); err != nil {
		return "", fmt.Errorf("finding next local issue number for project %s: %w", project, err)
	}

	key := fmt.Sprintf("%s-%d", project, maxN.Int64+1)

	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return "", fmt.Errorf("marshaling labels for %s: %w", key, err)
	}

	_, err = s.db.Exec(
		`INSERT INTO local_issues (key, project, summary, description, issue_type, labels)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		key, project, summary, nullable(description), issueType, string(labelsJSON),
	)
	if err != nil {
		return "", fmt.Errorf("inserting local issue %s: %w", key, err)
	}

	return key, nil
}

// GetLocalIssue returns the local issue with the given key, or
// ErrLocalIssueNotFound if none exists.
func (s *Store) GetLocalIssue(key string) (LocalIssue, error) {
	var (
		issue       LocalIssue
		description sql.NullString
		labelsJSON  string
	)

	err := s.db.QueryRow(
		`SELECT key, project, summary, description, issue_type, status_category, labels
		 FROM local_issues WHERE key = ?`,
		key,
	).Scan(&issue.Key, &issue.Project, &issue.Summary, &description, &issue.IssueType, &issue.StatusCategory, &labelsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return LocalIssue{}, fmt.Errorf("getting local issue %s: %w", key, ErrLocalIssueNotFound)
	}
	if err != nil {
		return LocalIssue{}, fmt.Errorf("getting local issue %s: %w", key, err)
	}

	issue.Description = description.String

	if err := json.Unmarshal([]byte(labelsJSON), &issue.Labels); err != nil {
		return LocalIssue{}, fmt.Errorf("unmarshaling labels for %s: %w", key, err)
	}

	return issue, nil
}

// SetLocalIssueStatus updates the status category for the local issue with
// the given key, or returns ErrLocalIssueNotFound if none exists.
func (s *Store) SetLocalIssueStatus(key, statusCategory string) error {
	res, err := s.db.Exec(
		`UPDATE local_issues
		 SET status_category = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
		 WHERE key = ?`,
		statusCategory, key,
	)
	if err != nil {
		return fmt.Errorf("setting status for local issue %s: %w", key, err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected for local issue %s: %w", key, err)
	}
	if rows == 0 {
		return fmt.Errorf("setting status for local issue %s: %w", key, ErrLocalIssueNotFound)
	}

	return nil
}

// InsertLocalIssueComment adds a comment to the local issue with the given
// key, or returns ErrLocalIssueNotFound if none exists.
func (s *Store) InsertLocalIssueComment(issueKey, body string) error {
	if _, err := s.GetLocalIssue(issueKey); err != nil {
		return fmt.Errorf("adding comment to local issue %s: %w", issueKey, err)
	}

	if _, err := s.db.Exec(
		`INSERT INTO local_issue_comments (issue_key, body) VALUES (?, ?)`,
		issueKey, body,
	); err != nil {
		return fmt.Errorf("adding comment to local issue %s: %w", issueKey, err)
	}

	return nil
}

// LocalIssueComments returns every comment body for issueKey, oldest first.
func (s *Store) LocalIssueComments(issueKey string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT body FROM local_issue_comments WHERE issue_key = ? ORDER BY id`,
		issueKey,
	)
	if err != nil {
		return nil, fmt.Errorf("querying comments for local issue %s: %w", issueKey, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, fmt.Errorf("scanning comment row for %s: %w", issueKey, err)
		}
		out = append(out, body)
	}

	return out, rows.Err()
}

// SearchLocalIssues returns up to limit local issues whose summary contains
// query (case-insensitive substring match; empty query matches all),
// ordered by key.
func (s *Store) SearchLocalIssues(query string, limit int) ([]LocalIssue, error) {
	rows, err := s.db.Query(
		`SELECT key, project, summary, description, issue_type, status_category, labels
		 FROM local_issues WHERE summary LIKE '%' || ? || '%' COLLATE NOCASE
		 ORDER BY key LIMIT ?`,
		query, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("searching local issues for %q: %w", query, err)
	}
	defer func() { _ = rows.Close() }()

	var out []LocalIssue
	for rows.Next() {
		var (
			issue       LocalIssue
			description sql.NullString
			labelsJSON  string
		)
		if err := rows.Scan(
			&issue.Key, &issue.Project, &issue.Summary, &description,
			&issue.IssueType, &issue.StatusCategory, &labelsJSON,
		); err != nil {
			return nil, fmt.Errorf("scanning local issue row: %w", err)
		}

		issue.Description = description.String
		if err := json.Unmarshal([]byte(labelsJSON), &issue.Labels); err != nil {
			return nil, fmt.Errorf("unmarshaling labels for %s: %w", issue.Key, err)
		}

		out = append(out, issue)
	}

	return out, rows.Err()
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
// them, instead of persisting an empty string.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
