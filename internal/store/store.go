// Package store provides SQLite persistence: events, collector cursors, and
// the phase-1+ tables.
//
// The event log is append-only; (source, external_id) is the dedup key so
// collectors can safely re-emit. Narratives/actions/estimates/ledger are
// created now so the schema is stable, but only events and cursors are
// written in phase 0.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

// ErrNarrativeNotFound is returned by GetNarrative when no row matches.
var ErrNarrativeNotFound = errors.New("narrative not found")

// ErrEventNotFound is returned by EventIDByExternalID when no event matches
// the given (source, external_id).
var ErrEventNotFound = errors.New("event not found")

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
    -- Paired with compaction_boundary to break ties: occurred_at alone
    -- cannot uniquely order events (it's stored via time.RFC3339, whole
    -- seconds only), so NarrativeEventsForContext compares the
    -- (occurred_at, event_id) pair — events.id is a monotonic
    -- INTEGER PRIMARY KEY, an exact tiebreaker for events sharing a second.
    compaction_boundary_event_id INTEGER,
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

CREATE TABLE IF NOT EXISTS pipeline_lock (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    run_id     TEXT NOT NULL,
    held_since TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
`

// Store wraps a SQLite connection with unjira's schema and access methods.
type Store struct {
	db *sql.DB
}

// dbConn is the subset of *sql.DB / *sql.Tx the narrative accessors need, so
// each accessor's SQL body can run either directly (*Store) or inside a
// transaction (*Tx) without duplicating the query logic.
type dbConn interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

// Tx is a transaction-scoped handle exposing the narrative write/read methods
// correlator.Persist needs to run atomically. Obtain one via WithTx.
type Tx struct {
	tx *sql.Tx
}

// WithTx runs fn inside a single transaction, committing if fn returns nil and
// rolling back (preserving fn's error) otherwise. This is how Persist gets its
// all-or-nothing guarantee.
//
// There is no defer-based rollback guard: a panic inside fn propagates without
// an immediate Rollback, leaving the transaction to be rolled back by its
// context-cancellation goroutine when the *sql.Tx is GC'd. That is acceptable
// for unjira's single-shot, lease-serialized batch model (a panic crashes the
// process anyway); revisit if this ever runs inside a longer-lived process that
// recovers panics.
func (s *Store) WithTx(fn func(*Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}

	if err := fn(&Tx{tx: tx}); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			// Wrap both: callers may need errors.Is against either the
			// original failure or the rollback failure that masked it.
			return fmt.Errorf("rolling back after error %w: %w", err, rbErr)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
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

	if err := ensureNarrativeColumn(db, "compaction_boundary", "TEXT"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating narratives.compaction_boundary in %s: %w", dbPath, err)
	}
	if err := ensureNarrativeColumn(db, "compaction_boundary_event_id", "INTEGER"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating narratives.compaction_boundary_event_id in %s: %w", dbPath, err)
	}

	return &Store{db: db}, nil
}

// ensureNarrativeColumn adds the named column (with the given SQL type) to a
// database whose narratives table predates it. CREATE TABLE IF NOT EXISTS is
// a no-op on an existing table, so a fresh DB gets every column already
// listed in the schema string while an older DB needs this explicit ALTER
// for anything added after it was created. Idempotent: it checks column
// presence first (via PRAGMA table_info, never by matching an ALTER's error
// string) and does nothing when already present. Shared by both
// compaction_boundary (Task 4) and compaction_boundary_event_id (this
// change) so a third such column doesn't need its own bespoke migration.
func ensureNarrativeColumn(db *sql.DB, column, sqlType string) error {
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
		if name == column {
			return rows.Err() // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE narratives ADD COLUMN %s %s`, column, sqlType)); err != nil {
		return fmt.Errorf("adding %s column: %w", column, err)
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

// -- narratives ----------------------------------------------------------

// NarrativeRow mirrors a narratives table row. correlator.Persist maps this
// to/from its own correlator.Narrative domain type (keeping store free of any
// correlator import — the dependency runs correlator -> store).
type NarrativeRow struct {
	ID                 int64
	WindowStart        time.Time
	WindowEnd          time.Time
	Title              string
	Summary            string
	IssueKey           string
	Confidence         float64
	Status             string
	CompactionBoundary *time.Time
	// CompactionBoundaryEventID pairs with CompactionBoundary to break ties:
	// occurred_at alone cannot uniquely order events (stored via
	// time.RFC3339 — whole seconds only), so NarrativeEventsForContext
	// filters on the (occurred_at, event_id) pair rather than occurred_at
	// alone. nil iff CompactionBoundary is nil (never compacted).
	CompactionBoundaryEventID *int64
}

// InsertNarrative inserts a new narrative row (status 'open', no compaction
// boundary) and returns its id.
func (s *Store) InsertNarrative(windowStart, windowEnd time.Time, title, summary string) (int64, error) {
	return insertNarrativeImpl(s.db, windowStart, windowEnd, title, summary)
}

// InsertNarrative is the *Tx-scoped variant of (*Store).InsertNarrative.
func (t *Tx) InsertNarrative(windowStart, windowEnd time.Time, title, summary string) (int64, error) {
	return insertNarrativeImpl(t.tx, windowStart, windowEnd, title, summary)
}

func insertNarrativeImpl(c dbConn, windowStart, windowEnd time.Time, title, summary string) (int64, error) {
	res, err := c.Exec(
		`INSERT INTO narratives (window_start, window_end, title, summary) VALUES (?, ?, ?, ?)`,
		windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339), title, summary,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting narrative %q: %w", title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting inserted narrative id for %q: %w", title, err)
	}

	return id, nil
}

// GetNarrative returns the narrative with the given id, or
// ErrNarrativeNotFound.
func (s *Store) GetNarrative(id int64) (NarrativeRow, error) {
	return getNarrativeImpl(s.db, id)
}

// GetNarrative is the *Tx-scoped variant of (*Store).GetNarrative.
func (t *Tx) GetNarrative(id int64) (NarrativeRow, error) {
	return getNarrativeImpl(t.tx, id)
}

func getNarrativeImpl(c dbConn, id int64) (NarrativeRow, error) {
	var (
		row                NarrativeRow
		windowStart        string
		windowEnd          string
		issueKey           sql.NullString
		confidence         sql.NullFloat64
		compactionBoundary sql.NullString
		compactionEventID  sql.NullInt64
	)
	err := c.QueryRow(
		`SELECT id, window_start, window_end, title, summary, issue_key, confidence, status,
		        compaction_boundary, compaction_boundary_event_id
		 FROM narratives WHERE id = ?`, id,
	).Scan(&row.ID, &windowStart, &windowEnd, &row.Title, &row.Summary,
		&issueKey, &confidence, &row.Status, &compactionBoundary, &compactionEventID)
	if errors.Is(err, sql.ErrNoRows) {
		return NarrativeRow{}, ErrNarrativeNotFound
	}
	if err != nil {
		return NarrativeRow{}, fmt.Errorf("querying narrative %d: %w", id, err)
	}

	if row.WindowStart, err = time.Parse(time.RFC3339, windowStart); err != nil {
		return NarrativeRow{}, fmt.Errorf("parsing window_start for narrative %d: %w", id, err)
	}
	if row.WindowEnd, err = time.Parse(time.RFC3339, windowEnd); err != nil {
		return NarrativeRow{}, fmt.Errorf("parsing window_end for narrative %d: %w", id, err)
	}
	row.IssueKey = issueKey.String
	if confidence.Valid {
		row.Confidence = confidence.Float64
	}
	if compactionBoundary.Valid {
		parsed, err := time.Parse(time.RFC3339, compactionBoundary.String)
		if err != nil {
			return NarrativeRow{}, fmt.Errorf("parsing compaction_boundary for narrative %d: %w", id, err)
		}
		row.CompactionBoundary = &parsed
	}
	if compactionEventID.Valid {
		row.CompactionBoundaryEventID = &compactionEventID.Int64
	}

	return row, nil
}

// ExtendNarrative advances a narrative's window_end and overwrites its
// summary.
func (s *Store) ExtendNarrative(id int64, windowEnd time.Time, summary string) error {
	return extendNarrativeImpl(s.db, id, windowEnd, summary)
}

// ExtendNarrative is the *Tx-scoped variant of (*Store).ExtendNarrative.
func (t *Tx) ExtendNarrative(id int64, windowEnd time.Time, summary string) error {
	return extendNarrativeImpl(t.tx, id, windowEnd, summary)
}

func extendNarrativeImpl(c dbConn, id int64, windowEnd time.Time, summary string) error {
	_, err := c.Exec(
		`UPDATE narratives SET window_end = ?, summary = ? WHERE id = ?`,
		windowEnd.Format(time.RFC3339), summary, id,
	)
	if err != nil {
		return fmt.Errorf("extending narrative %d: %w", id, err)
	}

	return nil
}

// SetCompactionBoundary records the occurred_at and row id of the newest
// compacted event and stores the recap-prefixed summary. boundaryEventID is
// required alongside boundary: occurred_at alone cannot uniquely order
// events sharing a stored second (time.RFC3339 truncates to whole seconds —
// see the comment on NarrativeEventsForContext), so the pair is what
// NarrativeEventsForContext's row-value comparison uses to avoid dropping a
// tied event from future context.
func (s *Store) SetCompactionBoundary(id int64, boundary time.Time, boundaryEventID int64, recapSummary string) error {
	return setCompactionBoundaryImpl(s.db, id, boundary, boundaryEventID, recapSummary)
}

// SetCompactionBoundary is the *Tx-scoped variant of
// (*Store).SetCompactionBoundary.
func (t *Tx) SetCompactionBoundary(id int64, boundary time.Time, boundaryEventID int64, recapSummary string) error {
	return setCompactionBoundaryImpl(t.tx, id, boundary, boundaryEventID, recapSummary)
}

func setCompactionBoundaryImpl(c dbConn, id int64, boundary time.Time, boundaryEventID int64, recapSummary string) error {
	_, err := c.Exec(
		`UPDATE narratives SET compaction_boundary = ?, compaction_boundary_event_id = ?, summary = ? WHERE id = ?`,
		boundary.Format(time.RFC3339), boundaryEventID, recapSummary, id,
	)
	if err != nil {
		return fmt.Errorf("setting compaction boundary for narrative %d: %w", id, err)
	}

	return nil
}

// AddNarrativeEvents links events to a narrative (INSERT OR IGNORE, so
// re-linking an already-linked event is a harmless no-op).
func (s *Store) AddNarrativeEvents(narrativeID int64, eventIDs []int64) error {
	return addNarrativeEventsImpl(s.db, narrativeID, eventIDs)
}

// AddNarrativeEvents is the *Tx-scoped variant of
// (*Store).AddNarrativeEvents.
func (t *Tx) AddNarrativeEvents(narrativeID int64, eventIDs []int64) error {
	return addNarrativeEventsImpl(t.tx, narrativeID, eventIDs)
}

func addNarrativeEventsImpl(c dbConn, narrativeID int64, eventIDs []int64) error {
	for _, eid := range eventIDs {
		if _, err := c.Exec(
			`INSERT OR IGNORE INTO narrative_events (narrative_id, event_id) VALUES (?, ?)`,
			narrativeID, eid,
		); err != nil {
			return fmt.Errorf("linking event %d to narrative %d: %w", eid, narrativeID, err)
		}
	}

	return nil
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

// NarrativeEventsForContext returns a narrative's events strictly after its
// compaction boundary (all of them when the boundary is NULL), ordered by
// (occurred_at, event id) — the events the caller hydrates into
// correlator.Narrative.Events. The recap of anything at/before the boundary
// already lives in the summary.
//
// The boundary comparison is on the pair (compaction_boundary,
// compaction_boundary_event_id), not occurred_at alone: occurred_at is
// stored via time.RFC3339 (whole seconds only — see InsertEvent), so two
// events in the same second are indistinguishable by timestamp. A bare
// "occurred_at > boundary" filter would then either include or exclude
// *both* tied events depending on which one the boundary happened to be set
// from, silently dropping whichever tied event was meant to stay visible.
// events.id is a monotonic INTEGER PRIMARY KEY, so pairing it with
// occurred_at makes the ordering exact regardless of timestamp collisions
// (this is also why compaction picks its boundary event's row id, not just
// its timestamp — see correlator.compactNarrativeTail).
//
// Uses a SQL row-value comparison ("(a, b) > (x, y)"), verified against
// modernc.org/sqlite (the driver this package uses) with a standalone
// scratch program before relying on it here; modernc.org/sqlite is well
// past the SQLite 3.15 baseline that introduced row values. The explicit
// equivalent ("a > x OR (a = x AND b > y)") is the documented fallback if a
// future driver swap ever regresses this.
//
// A narrative id with no matching row (or one with no linked events)
// returns (nil, nil), not an error — callers only invoke this with an id
// they already obtained from the store.
func (s *Store) NarrativeEventsForContext(narrativeID int64) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM events e
		 JOIN narrative_events ne ON ne.event_id = e.id
		 WHERE ne.narrative_id = ?
		   AND (
		     (SELECT compaction_boundary FROM narratives WHERE id = ?) IS NULL
		     OR (e.occurred_at, e.id) > (
		       (SELECT compaction_boundary FROM narratives WHERE id = ?),
		       (SELECT compaction_boundary_event_id FROM narratives WHERE id = ?)
		     )
		   )
		 ORDER BY e.occurred_at, e.id`,
		narrativeID, narrativeID, narrativeID, narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying context events for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []events.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning context event row: %w", err)
		}
		out = append(out, e)
	}

	return out, rows.Err()
}

// NarrativeEventCount returns how many events are linked to a narrative,
// ignoring its compaction boundary — unlike NarrativeEventsForContext, which
// returns only the post-boundary tail. This is a test-support introspection
// accessor (see NarrativeColumns for the precedent): it's what lets a test
// prove the "narrative_events rows are never deleted" invariant, since
// compaction shrinks the assembled context, never the links.
func (s *Store) NarrativeEventCount(narrativeID int64) (int, error) {
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM narrative_events WHERE narrative_id = ?`, narrativeID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting linked events for narrative %d: %w", narrativeID, err)
	}

	return count, nil
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

// -- pipeline lock ---------------------------------------------------------

// lockTimeFormat is the timestamp layout used for pipeline_lock.held_since
// and pipeline_lock.expires_at only — NOT the layout used elsewhere in this
// package (events/narratives/cursors stay on time.RFC3339; changing those
// would be a migration).
//
// It differs from time.RFC3339 in two ways, both required for TryAcquire's
// atomic steal guard (a SQL string comparison, not a time comparison):
//
//   - Fixed sub-second width. time.RFC3339 has no fractional-second
//     placeholder at all, so Format silently truncates to whole seconds —
//     any sub-second TTL becomes indistinguishable from "already expired."
//     time.RFC3339Nano fixes precision but strips trailing zeros, which
//     breaks the lexicographic-order property below.
//   - Lexicographic order equals chronological order. A fixed-width
//     fractional part (always 9 digits) means comparing the formatted
//     strings byte-by-byte gives the same answer as comparing the times.
//     "...12:00:00.100000000Z" < "...12:00:00.150000000Z" holds as strings
//     exactly because both are always 9 digits; RFC3339Nano would format
//     these as ".1" and ".15", where the string comparison is wrong.
//
// TryAcquire normalizes now to UTC before formatting so two callers with
// the same instant but different offsets can't produce different Z07:00
// suffixes and defeat the ordering.
const lockTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// TryAcquire attempts to take the singleton pipeline lock without blocking,
// in a single atomic statement (safe across concurrent processes/connections,
// unlike a separate read-then-write: two callers observing "unheld" could
// otherwise both upsert and both be told they acquired it). It succeeds when
// the lock is unheld or its lease has expired (expires_at at or before now),
// replacing the row with a fresh lease for runID; otherwise it returns false
// immediately. now is passed in (not time.Now()) so steal-on-expiry is
// deterministically testable.
//
// Stealing an expired lease logs a warning naming the stale run_id and how
// long it was held — the crash-recovery path. That diagnostic comes from a
// plain SELECT taken just before the atomic statement; it is best-effort
// (another acquirer could race between the SELECT and the write) and never
// gates the outcome — only the atomic statement's own RowsAffected does that.
func (s *Store) TryAcquire(runID string, now time.Time, ttl time.Duration) (bool, error) {
	now = now.UTC()
	nowStr := now.Format(lockTimeFormat)

	var (
		priorRunID string
		priorExp   string
	)
	if err := s.db.QueryRow(
		`SELECT run_id, expires_at FROM pipeline_lock WHERE id = 1`,
	).Scan(&priorRunID, &priorExp); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("reading pipeline lock for diagnostics: %w", err)
	}

	res, err := s.db.Exec(
		`INSERT INTO pipeline_lock (id, run_id, held_since, expires_at) VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		     run_id = excluded.run_id, held_since = excluded.held_since, expires_at = excluded.expires_at
		 WHERE pipeline_lock.expires_at <= ?`,
		runID, nowStr, now.Add(ttl).Format(lockTimeFormat), nowStr,
	)
	if err != nil {
		return false, fmt.Errorf("acquiring pipeline lock for %s: %w", runID, err)
	}

	acquired, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking rows affected acquiring pipeline lock for %s: %w", runID, err)
	}
	if acquired == 0 {
		return false, nil // still held by someone else
	}

	if priorRunID != "" && priorRunID != runID {
		if priorExpTime, perr := time.Parse(lockTimeFormat, priorExp); perr == nil {
			log.Printf("pipeline lock: stealing expired lease from run_id=%s (expired %s ago)",
				priorRunID, now.Sub(priorExpTime))
		}
	}

	return true, nil
}

// Acquire is TryAcquire's blocking sibling: it polls every poll interval
// until it can take the lock (unheld or lease expired), honoring ctx
// cancellation. now is a clock func since Acquire loops. poll must be
// positive — a zero or negative poll turns this into a hot loop.
func (s *Store) Acquire(ctx context.Context, runID string, now func() time.Time, ttl, poll time.Duration) error {
	for {
		ok, err := s.TryAcquire(runID, now(), ttl)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("acquiring pipeline lock for %s: %w", runID, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// ReleaseLock releases the pipeline lock iff it is currently held by runID.
// Releasing when runID is not the holder is a no-op — not an error, since it
// never clobbers a newer holder that stole an expired lease — but it is
// logged, for the same reason TryAcquire logs a steal: it is a noteworthy
// runtime condition (a run trying to release a lock it no longer holds,
// e.g. because its lease already expired and was stolen out from under it)
// that operators should be able to see without it failing the caller.
func (s *Store) ReleaseLock(runID string) error {
	res, err := s.db.Exec(`DELETE FROM pipeline_lock WHERE id = 1 AND run_id = ?`, runID)
	if err != nil {
		return fmt.Errorf("releasing pipeline lock for %s: %w", runID, err)
	}

	released, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected releasing pipeline lock for %s: %w", runID, err)
	}
	if released == 0 {
		log.Printf("pipeline lock: release by run_id=%s was a no-op (not the current holder)", runID)
	}

	return nil
}
