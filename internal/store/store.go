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
	"strings"
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

// ErrActionNotFound is returned by GetAction when no row matches — `unjira
// actions decide` needs to distinguish "no such action" from every other
// failure mode, since the former is a user-facing error (a typo'd id), not
// a store bug.
var ErrActionNotFound = errors.New("action not found")

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
    -- Sub-second (%f, milliseconds), not %S. The reconciler's delta is
    -- linked_at > (last action's created_at); at whole-second granularity an
    -- event linked in the same second as the action is invisible forever,
    -- because that action's created_at never advances. actions.created_at uses
    -- this identical format on purpose: both are TEXT and compared lexically,
    -- and mixing %f with %S inverts the comparison ('.' 0x2E sorts before
    -- 'Z' 0x5A), so a later event would read as earlier.
    linked_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (narrative_id, event_id)
);

CREATE TABLE IF NOT EXISTS narrative_issues (
    narrative_id INTEGER NOT NULL REFERENCES narratives (id),
    issue_key    TEXT    NOT NULL,
    role         TEXT    NOT NULL,   -- primary | same_work | mentioned
    provenance   TEXT    NOT NULL,   -- branch | jira_event | prose_first | prose_later
    confidence   REAL,
    connection   TEXT,               -- which JiraConnection resolved it
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (narrative_id, issue_key)
);

-- Exactly one primary per narrative, enforced by the database rather than by
-- convention: narratives.issue_key denormalizes the primary, so a second
-- primary row would make that column arbitrary. The composite PRIMARY KEY
-- above prevents duplicate keys per narrative but permits two rows both marked
-- primary, which is the case this closes.
CREATE UNIQUE INDEX IF NOT EXISTS one_primary_per_narrative
    ON narrative_issues (narrative_id) WHERE role = 'primary';

-- Supports the reverse lookup (issue_key -> narratives), which is otherwise
-- unindexed since issue_key is the trailing column of the composite key.
CREATE INDEX IF NOT EXISTS narrative_issues_by_key
    ON narrative_issues (issue_key);

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
    -- Written by slice 6's triage/rework loop, nothing today. Added now so
    -- that slice does not need a second schema change (see the phase-1 spec's
    -- schema-additions section, where it was specified but never landed).
    feedback     TEXT,
    -- Same %f format as narrative_events.linked_at, and for the same reason:
    -- DeltaEvents compares these two TEXT columns lexically
    -- (linked_at > created_at), so both must share the identical format
    -- string or the comparison silently inverts.
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
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

	db, err := sql.Open("sqlite", sqliteDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", dbPath, err)
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("applying schema to %s: %w", dbPath, err)
	}

	return &Store{db: db}, nil
}

// sqliteDSN turns a plain filesystem path into a modernc.org/sqlite DSN that
// turns on foreign-key enforcement.
//
// With this driver, `PRAGMA foreign_keys = ON` is per-connection, and
// database/sql maintains a *pool* of connections — a one-shot
// db.Exec("PRAGMA foreign_keys = ON") only reaches whichever single
// connection happens to run it, leaving every other pooled connection
// (including ones opened later, under concurrent load) unenforced. That is
// silent and asymmetric with the failure it's meant to catch: a single-
// threaded test would pass while production, which opens more than one
// connection, would not enforce the constraint at all. The fix is to put
// the pragma in the DSN itself (`_pragma=foreign_keys(1)`), which
// modernc.org/sqlite applies to every connection it opens, not just the
// first.
//
// The `_pragma` query parameter is only honored in the `file:` URI form, so
// the path has to be escaped for exactly the three characters that are
// structural in a DSN — and for nothing else. Each replacement was confirmed
// by opening real databases with an unescaped `file:`+path DSN:
//
//	wi?rd.db     →  wrote to "wi",       foreign_keys OFF
//	wi#rd.db     →  wrote to "wi",       foreign_keys on
//	wi%3frd.db   →  wrote to "wi?rd.db", foreign_keys on
//
// '?' splits path from query, so both the filename and the pragma are lost —
// silently disabling the very enforcement this function exists to guarantee.
// '#' truncates the path as a fragment. '%' must be escaped FIRST (as it is)
// because the other two introduce percent-escapes: without it, a db_path
// already containing "%3f" round-trips into a literal '?' and unjira writes
// to a file the operator never named.
//
// Do NOT replace this with a general URL encoder — both obvious candidates
// are wrong here, because they don't know '/' is load-bearing and '?' is not:
//
//   - net/url's url.URL renders a *relative* Path with a "//" authority
//     prefix: `file://data/unjira.db`, which makes "data" the URL authority
//     rather than a directory. That fails at the first Exec with
//     "SQL logic error: out of memory (1)". config/unjira.example.json ships
//     db_path "data/unjira.db", so this breaks the default configuration.
//   - url.PathEscape escapes '/' too, collapsing the path into one filename
//     component.
//
// So '/' stays unescaped deliberately, and absolute, "./relative", and bare
// "relative" forms were each verified to open the intended file with
// foreign_keys on.
func sqliteDSN(dbPath string) string {
	p := filepath.ToSlash(dbPath)
	p = strings.ReplaceAll(p, "%", "%25")
	p = strings.ReplaceAll(p, "?", "%3f")
	p = strings.ReplaceAll(p, "#", "%23")

	return "file:" + p + "?_pragma=foreign_keys(1)"
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
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
	row, err := scanNarrativeRow(c.QueryRow(
		`SELECT id, window_start, window_end, title, summary, issue_key, confidence, status,
		        compaction_boundary, compaction_boundary_event_id
		 FROM narratives WHERE id = ?`, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return NarrativeRow{}, ErrNarrativeNotFound
	}
	if err != nil {
		return NarrativeRow{}, fmt.Errorf("querying narrative %d: %w", id, err)
	}

	return row, nil
}

// scanNarrativeRow scans one narratives row (in the column order
// id, window_start, window_end, title, summary, issue_key, confidence,
// status, compaction_boundary, compaction_boundary_event_id) and parses its
// RFC3339 timestamps. Shared by GetNarrative (single row, via *sql.Row) and
// NarrativesOverlapping (many, via *sql.Rows) so the nullable-column handling
// exists once. Takes scanRow rather than a concrete type for exactly that
// reason — both *sql.Row and *sql.Rows satisfy it, mirroring scanEvent.
func scanNarrativeRow(row scanRow) (NarrativeRow, error) {
	var (
		out                NarrativeRow
		windowStart        string
		windowEnd          string
		issueKey           sql.NullString
		confidence         sql.NullFloat64
		compactionBoundary sql.NullString
		compactionEventID  sql.NullInt64
	)

	if err := row.Scan(&out.ID, &windowStart, &windowEnd, &out.Title, &out.Summary,
		&issueKey, &confidence, &out.Status, &compactionBoundary, &compactionEventID); err != nil {
		return NarrativeRow{}, err
	}

	var err error
	if out.WindowStart, err = time.Parse(time.RFC3339, windowStart); err != nil {
		return NarrativeRow{}, fmt.Errorf("parsing window_start for narrative %d: %w", out.ID, err)
	}
	if out.WindowEnd, err = time.Parse(time.RFC3339, windowEnd); err != nil {
		return NarrativeRow{}, fmt.Errorf("parsing window_end for narrative %d: %w", out.ID, err)
	}

	out.IssueKey = issueKey.String
	if confidence.Valid {
		out.Confidence = confidence.Float64
	}
	if compactionBoundary.Valid {
		parsed, perr := time.Parse(time.RFC3339, compactionBoundary.String)
		if perr != nil {
			return NarrativeRow{}, fmt.Errorf("parsing compaction_boundary for narrative %d: %w", out.ID, perr)
		}
		out.CompactionBoundary = &parsed
	}
	if compactionEventID.Valid {
		id := compactionEventID.Int64
		out.CompactionBoundaryEventID = &id
	}

	return out, nil
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

// UnlinkedEventsInRange returns events in [start, end) that are not yet
// linked to any narrative, ordered by (occurred_at, id) — the clustering
// candidates a narration pass considers.
//
// "Unlinked" means no narrative_events row at all, not "linked to a narrative
// outside this range": an event belongs to exactly one narrative, so once
// linked it is never a candidate again. Such an event can still reach a
// prompt as context via its narrative's hydration
// (NarrativeEventsForContext), which is why excluding it here does not starve
// the model.
//
// Ordering is composite because occurred_at is stored via time.RFC3339
// (whole seconds — see InsertEvent) and cannot uniquely order events.
func (s *Store) UnlinkedEventsInRange(start, end time.Time) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM events e
		 WHERE e.occurred_at >= ? AND e.occurred_at < ?
		   AND NOT EXISTS (SELECT 1 FROM narrative_events ne WHERE ne.event_id = e.id)
		 ORDER BY e.occurred_at, e.id`,
		start.Format(time.RFC3339), end.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("querying unlinked events in [%s, %s): %w",
			start.Format(time.RFC3339), end.Format(time.RFC3339), err)
	}
	defer func() { _ = rows.Close() }()

	var out []events.Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning unlinked event row: %w", err)
		}
		out = append(out, event)
	}

	return out, rows.Err()
}

// NarrativesOverlapping returns narratives whose window overlaps or merely
// touches [start, end), ordered by (window_start, id) — the context a
// narration pass passes to Cluster.
//
// The predicate mirrors correlator's own adjacency filter: keep unless
// strictly disjoint (window_end < start || window_start > end). Touching
// endpoints count as adjacent deliberately — temporal proximity is real
// clustering signal, so a narrative ending exactly when this window opens is
// the most likely thing an early event extends.
//
// status is not filtered: every narrative is 'open' today and nothing sets
// otherwise, so filtering would be speculative. When status becomes
// meaningful, the predicate belongs here.
func (s *Store) NarrativesOverlapping(start, end time.Time) ([]NarrativeRow, error) {
	rows, err := s.db.Query(
		`SELECT id, window_start, window_end, title, summary, issue_key, confidence, status,
		        compaction_boundary, compaction_boundary_event_id
		 FROM narratives
		 WHERE window_end >= ? AND window_start <= ?
		 ORDER BY window_start, id`,
		start.Format(time.RFC3339), end.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives overlapping [%s, %s): %w",
			start.Format(time.RFC3339), end.Format(time.RFC3339), err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeRow
	for rows.Next() {
		row, err := scanNarrativeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning overlapping narrative row: %w", err)
		}
		out = append(out, row)
	}

	return out, rows.Err()
}

// -- narrative issues (narrative -> issue matching) -----------------------

// Role is which relationship a narrative has to an issue. A closed set: an
// unrecognized value is a parse error upstream, never persisted.
type Role string

// NarrativeIssue is one (narrative, issue) link — the narrative_issues row
// shape, mirroring how NarrativeRow mirrors narratives.
//
// There is deliberately no issue-to-issue column: every row relates a
// narrative to an issue, so co-representation of one body of work across two
// tickets is expressed by two rows sharing a narrative_id, and symmetry is
// derived rather than stored in two places that could disagree.
type NarrativeIssue struct {
	IssueKey string
	Role     Role
	// Provenance is a plain string rather than a typed enum: the typed
	// Provenance lives in internal/correlator, which this package must not
	// import, and a value read back from SQLite is untyped text regardless.
	Provenance string
	Confidence float64
	// Connection is the config.JiraConnection.Name that resolved this key,
	// recorded because a co-representation can live on a different site than
	// the primary.
	Connection string
}

// NarrativeIssueRef is one row of the reverse lookup: the caller already
// knows the issue key and needs the narrative.
type NarrativeIssueRef struct {
	NarrativeID int64
	Role        Role
	Confidence  float64
}

// NarrativesWithoutIssueKey returns up to limit narratives that have not yet
// been attributed to a primary issue, ordered by (window_start, id) — the
// backlog a matching pass works through. InsertNarrative leaves issue_key
// NULL (the column has no NOT NULL constraint and is omitted from the insert
// column list), so "IS NULL" alone would suffice; the "= ”" half of the
// predicate is defensive belt-and-suspenders in case a future writer ever
// persists an empty string instead of leaving it NULL.
func (s *Store) NarrativesWithoutIssueKey(limit int) ([]NarrativeRow, error) {
	rows, err := s.db.Query(
		`SELECT id, window_start, window_end, title, summary, issue_key, confidence, status,
		        compaction_boundary, compaction_boundary_event_id
		 FROM narratives
		 WHERE issue_key IS NULL OR issue_key = ''
		 ORDER BY window_start, id
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives without an issue key: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeRow
	for rows.Next() {
		row, err := scanNarrativeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning unmatched narrative row: %w", err)
		}
		out = append(out, row)
	}

	return out, rows.Err()
}

// NarrativesWithActionableLinks returns up to limit narratives having at
// least one narrative_issues link whose role is in roles, ordered by
// (window_start, id) — the reconciler's input backlog.
//
// Deliberately NOT keyed on the denormalized narratives.issue_key, which
// MatchConfig.ConfidenceFloor only promotes above the floor: a real but
// low-confidence primary has narrative_issues rows and a NULL issue_key, and
// selecting on issue_key would leave the reconciler permanently blind to it
// — exactly the class of bug behind the 08-21 discovery that
// narratives.issue_key was never written at all. Roles are passed in rather
// than hardcoded so the caller (reconciler.actionableLinks) stays the single
// definition of "actionable" — this package must not import
// internal/correlator.
//
// An empty roles slice returns an empty result (not an error): "nothing is
// actionable" is a valid caller configuration, not a malformed query.
func (s *Store) NarrativesWithActionableLinks(limit int, roles []Role) ([]NarrativeRow, error) {
	if len(roles) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(roles))
	args := make([]any, 0, len(roles)+1)
	for i, r := range roles {
		placeholders[i] = "?"
		args = append(args, string(r))
	}
	args = append(args, limit)

	query := `SELECT n.id, n.window_start, n.window_end, n.title, n.summary, n.issue_key,
	                 n.confidence, n.status, n.compaction_boundary, n.compaction_boundary_event_id
	          FROM narratives n
	          WHERE EXISTS (
	              SELECT 1 FROM narrative_issues ni
	              WHERE ni.narrative_id = n.id AND ni.role IN (` + strings.Join(placeholders, ",") + `)
	          )
	          ORDER BY n.window_start, n.id
	          LIMIT ?`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying narratives with actionable links: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeRow
	for rows.Next() {
		row, err := scanNarrativeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning actionable-linked narrative row: %w", err)
		}
		out = append(out, row)
	}

	return out, rows.Err()
}

// SetNarrativeIssueLink denormalizes a narrative's primary issue onto
// narratives.issue_key/confidence, so callers that only need "what issue is
// this" (digest/status output, NarrativesWithoutIssueKey's backlog filter)
// don't have to join narrative_issues. AddNarrativeIssues is what actually
// records the primary relationship; call both when setting a primary.
func (s *Store) SetNarrativeIssueLink(id int64, issueKey string, confidence float64) error {
	return setNarrativeIssueLinkImpl(s.db, id, issueKey, confidence)
}

// SetNarrativeIssueLink is the *Tx-scoped variant of
// (*Store).SetNarrativeIssueLink.
func (t *Tx) SetNarrativeIssueLink(id int64, issueKey string, confidence float64) error {
	return setNarrativeIssueLinkImpl(t.tx, id, issueKey, confidence)
}

func setNarrativeIssueLinkImpl(c dbConn, id int64, issueKey string, confidence float64) error {
	if _, err := c.Exec(
		`UPDATE narratives SET issue_key = ?, confidence = ? WHERE id = ?`,
		issueKey, confidence, id,
	); err != nil {
		return fmt.Errorf("setting issue link for narrative %d: %w", id, err)
	}

	return nil
}

// AddNarrativeIssues records narrative_issues rows for a narrative, one per
// link. Each is an explicit upsert keyed on (narrative_id, issue_key) — NOT
// INSERT OR IGNORE. That distinction is load-bearing: measured against
// modernc.org/sqlite, INSERT OR IGNORE against the partial unique index on
// role='primary' returns a nil error and silently keeps the FIRST primary
// when a second primary is attempted under a different issue_key. A wrong
// early match would then quietly outrank a later, correct one while the
// write reported success. The explicit "ON CONFLICT ... DO UPDATE" instead
// lets the composite-key conflict path refresh role/provenance/confidence on
// a re-run (matching improves as better information becomes available) while
// still letting the database's partial unique index reject a genuine second
// primary with a real error.
func (s *Store) AddNarrativeIssues(narrativeID int64, links []NarrativeIssue) error {
	return addNarrativeIssuesImpl(s.db, narrativeID, links)
}

// AddNarrativeIssues is the *Tx-scoped variant of
// (*Store).AddNarrativeIssues.
func (t *Tx) AddNarrativeIssues(narrativeID int64, links []NarrativeIssue) error {
	return addNarrativeIssuesImpl(t.tx, narrativeID, links)
}

func addNarrativeIssuesImpl(c dbConn, narrativeID int64, links []NarrativeIssue) error {
	for _, link := range links {
		if _, err := c.Exec(
			`INSERT INTO narrative_issues
			   (narrative_id, issue_key, role, provenance, confidence, connection)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (narrative_id, issue_key) DO UPDATE SET
			   role       = excluded.role,
			   provenance = excluded.provenance,
			   confidence = excluded.confidence,
			   connection = excluded.connection`,
			narrativeID, link.IssueKey, string(link.Role), link.Provenance,
			link.Confidence, nullable(link.Connection),
		); err != nil {
			return fmt.Errorf("adding issue link %s (%s) to narrative %d: %w",
				link.IssueKey, link.Role, narrativeID, err)
		}
	}

	return nil
}

// NarrativeIssues returns every issue link recorded for a narrative — the
// join-table rows a caller needs to see all roles (primary, same_work,
// mentioned) at once, which narratives.issue_key alone cannot express.
func (s *Store) NarrativeIssues(narrativeID int64) ([]NarrativeIssue, error) {
	rows, err := s.db.Query(
		`SELECT issue_key, role, provenance, confidence, connection
		 FROM narrative_issues WHERE narrative_id = ? ORDER BY issue_key`,
		narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying issue links for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeIssue
	for rows.Next() {
		var (
			link       NarrativeIssue
			role       string
			confidence sql.NullFloat64
			connection sql.NullString
		)
		if err := rows.Scan(&link.IssueKey, &role, &link.Provenance, &confidence, &connection); err != nil {
			return nil, fmt.Errorf("scanning issue link row for narrative %d: %w", narrativeID, err)
		}
		link.Role = Role(role)
		if confidence.Valid {
			link.Confidence = confidence.Float64
		}
		link.Connection = connection.String
		out = append(out, link)
	}

	return out, rows.Err()
}

// NarrativesForIssue returns every narrative linked to issueKey across all
// roles — the reverse lookup the reconciler needs so it doesn't act twice on
// one issue when two narratives share it (the SUMO co-representation case).
func (s *Store) NarrativesForIssue(issueKey string) ([]NarrativeIssueRef, error) {
	rows, err := s.db.Query(
		`SELECT narrative_id, role, confidence
		 FROM narrative_issues WHERE issue_key = ? ORDER BY narrative_id`,
		issueKey,
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives for issue %s: %w", issueKey, err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeIssueRef
	for rows.Next() {
		var (
			ref        NarrativeIssueRef
			role       string
			confidence sql.NullFloat64
		)
		if err := rows.Scan(&ref.NarrativeID, &role, &confidence); err != nil {
			return nil, fmt.Errorf("scanning narrative ref row for issue %s: %w", issueKey, err)
		}
		ref.Role = Role(role)
		if confidence.Valid {
			ref.Confidence = confidence.Float64
		}
		out = append(out, ref)
	}

	return out, rows.Err()
}

// AllNarrativeEvents returns every event ever linked to a narrative,
// ignoring the compaction boundary — deliberately NOT
// NarrativeEventsForContext, which exists to hide pre-boundary events from
// the model once their content is captured in the recap summary. Matching
// candidate keys come from event artifacts, and the git_branch artifact
// carrying the strongest provenance signal typically sits on a narrative's
// oldest events — exactly the ones compaction hides. A matching pass that
// used NarrativeEventsForContext would silently lose that signal for any
// narrative old enough to have been compacted.
func (s *Store) AllNarrativeEvents(narrativeID int64) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM events e
		 JOIN narrative_events ne ON ne.event_id = e.id
		 WHERE ne.narrative_id = ?
		 ORDER BY e.occurred_at, e.id`,
		narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all events for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []events.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning narrative event row: %w", err)
		}
		out = append(out, e)
	}

	return out, rows.Err()
}

// DeltaEvents returns the events linked to narrativeID since the most recent
// action proposed for it — "what's new since a reviewer last saw this."
//
// Bounded by the last action's created_at (not decided_at or executed_at)
// deliberately: created_at is always set, so a proposal sitting unreviewed in
// the queue still suppresses re-proposing its delta. decided_at is NULL while
// unreviewed, which would make every pass re-propose the same thing; and a
// rejected action never gets an executed_at, so bounding on that would
// re-propose a rejected action identically forever, giving the reviewer's "no"
// no weight. See the design spec's comparison table.
//
// With no prior action every linked event is returned (COALESCE to ""; all
// real timestamps sort above the empty string), which is the first-pass case:
// the whole narrative is the delta.
func (s *Store) DeltaEvents(narrativeID int64) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM narrative_events ne
		 JOIN events e ON e.id = ne.event_id
		 WHERE ne.narrative_id = ?
		   AND ne.linked_at > COALESCE(
		       (SELECT MAX(created_at) FROM actions WHERE narrative_id = ?), '')
		 ORDER BY e.occurred_at, e.id`,
		narrativeID, narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying delta events for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []events.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning delta event row: %w", err)
		}
		out = append(out, e)
	}

	return out, rows.Err()
}

// NarrativeEventLinkedAt returns the linked_at timestamp for one
// (narrativeID, eventID) narrative_events row. This is a test-support
// introspection accessor — same precedent as NarrativeEventCount — so tests
// outside this package (store_test) can assert on linked_at's format without
// reaching into *sql.DB directly.
func (s *Store) NarrativeEventLinkedAt(narrativeID, eventID int64) (string, error) {
	var linkedAt string
	if err := s.db.QueryRow(
		`SELECT linked_at FROM narrative_events WHERE narrative_id = ? AND event_id = ?`,
		narrativeID, eventID,
	).Scan(&linkedAt); err != nil {
		return "", fmt.Errorf("querying linked_at for narrative %d event %d: %w", narrativeID, eventID, err)
	}

	return linkedAt, nil
}

// NarrativeEventCount returns how many events are linked to a narrative,
// ignoring its compaction boundary — unlike NarrativeEventsForContext, which
// returns only the post-boundary tail. This is a test-support introspection
// accessor: it's what lets a test prove the "narrative_events rows are never
// deleted" invariant, since compaction shrinks the assembled context, never
// the links.
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

// -- actions ---------------------------------------------------------------

// ActionRow is one row of the actions table — a proposed, decided, or
// executed change to a tracker issue.
//
// IssueKey is empty for a `create` action (there is no key until it is
// applied). DecidedAt/ExecutedAt are *string rather than string because NULL
// is meaningful: NULL decided_at means "no human has ruled yet", which is
// distinct from any timestamp. They stay strings rather than time.Time to
// match how the rest of this package stores timestamps (SQLite TEXT), and
// because nothing orders by them.
// JSON tags mirror the actions table's own column names (snake_case), not
// Go's default CamelCase field names — `unjira actions list --json` is a
// machine-facing surface built for scripting/jq, and a reader piping this
// output should see the same names they'd see in a `sqlite3` query, not a
// second, Go-flavored vocabulary for the same columns.
type ActionRow struct {
	ID          int64   `json:"id"`
	NarrativeID int64   `json:"narrative_id"`
	Type        string  `json:"type"` // comment | transition | create | estimate
	IssueKey    string  `json:"issue_key"`
	Payload     string  `json:"payload"` // JSON; shape depends on Type
	Confidence  float64 `json:"confidence"`
	Rationale   string  `json:"rationale"`
	Status      string  `json:"status"` // proposed | approved | edited | rejected | applied | failed
	Feedback    string  `json:"feedback"`
	DecidedAt   *string `json:"decided_at"`
	ExecutedAt  *string `json:"executed_at"`
	CreatedAt   string  `json:"created_at"`
}

// InsertAction writes one proposed action and returns its id. created_at
// comes from the column DEFAULT so it shares narrative_events.linked_at's
// format exactly — see DeltaEvents.
func (s *Store) InsertAction(a ActionRow) (int64, error) {
	return insertActionImpl(s.db, a)
}

// InsertAction is the Tx-scoped form, so reconciler.Persist can write a whole
// pass atomically.
func (t *Tx) InsertAction(a ActionRow) (int64, error) {
	return insertActionImpl(t.tx, a)
}

func insertActionImpl(c dbConn, a ActionRow) (int64, error) {
	res, err := c.Exec(
		`INSERT INTO actions
		   (narrative_id, type, issue_key, payload, confidence, rationale, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.NarrativeID, a.Type, nullable(a.IssueKey), a.Payload,
		a.Confidence, nullable(a.Rationale), a.Status,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting %s action for narrative %d: %w", a.Type, a.NarrativeID, err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading inserted action id: %w", err)
	}

	return id, nil
}

// ActionsForNarrative returns every action ever proposed for a narrative,
// oldest first.
func (s *Store) ActionsForNarrative(narrativeID int64) ([]ActionRow, error) {
	rows, err := s.db.Query(actionSelect+` WHERE narrative_id = ? ORDER BY created_at, id`, narrativeID)
	if err != nil {
		return nil, fmt.Errorf("querying actions for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	return scanActions(rows)
}

// ActionsByStatus returns every action in one workflow state, oldest first —
// the review queue's read path (slice 6's `triage`).
func (s *Store) ActionsByStatus(status string) ([]ActionRow, error) {
	rows, err := s.db.Query(actionSelect+` WHERE status = ? ORDER BY created_at, id`, status)
	if err != nil {
		return nil, fmt.Errorf("querying actions with status %q: %w", status, err)
	}
	defer func() { _ = rows.Close() }()

	return scanActions(rows)
}

// GetAction returns the action with the given id, or ErrActionNotFound.
//
// `unjira actions decide` is the first caller: it is handed a bare id on the
// command line and must load the full row (status, for the double-post
// guard; type/issue_key/payload, for Applier.Apply) before it can act on it
// at all. ActionsForNarrative/ActionsByStatus both require already knowing
// something else about the row (its narrative, its status) — this is the
// first accessor that takes only an id, matching how a human refers to a
// row in this table (they see an id in `actions list`, not a narrative id).
func (s *Store) GetAction(id int64) (ActionRow, error) {
	rows, err := s.db.Query(actionSelect+` WHERE id = ?`, id)
	if err != nil {
		return ActionRow{}, fmt.Errorf("querying action %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	found, err := scanActions(rows)
	if err != nil {
		return ActionRow{}, err
	}
	if len(found) == 0 {
		return ActionRow{}, fmt.Errorf("getting action %d: %w", id, ErrActionNotFound)
	}

	return found[0], nil
}

// LatestActionForNarrative returns the most recent action for a narrative.
//
// "Most recent" is by (created_at, id): created_at alone cannot order two
// actions written in the same millisecond, and id is monotonic. found is
// false with a nil error when the narrative has no actions at all — the
// first-pass case, not an error.
func (s *Store) LatestActionForNarrative(narrativeID int64) (ActionRow, bool, error) {
	rows, err := s.db.Query(
		actionSelect+` WHERE narrative_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		narrativeID,
	)
	if err != nil {
		return ActionRow{}, false, fmt.Errorf("querying latest action for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	found, err := scanActions(rows)
	if err != nil {
		return ActionRow{}, false, err
	}
	if len(found) == 0 {
		return ActionRow{}, false, nil
	}

	return found[0], true, nil
}

// UpdateActionStatus moves an action to a new workflow state, stamping
// decided_at and/or executed_at as that state implies.
//
// Both stamps are set with the same strftime format the columns default to, so
// every timestamp in this table remains directly comparable. Terminal-state
// semantics: any human ruling sets decided_at; only a write that actually
// reached the tracker sets executed_at.
func (s *Store) UpdateActionStatus(id int64, status string) error {
	return updateActionStatusImpl(s.db, id, status, nil)
}

// UpdateActionStatusAndFeedback moves an action to a new workflow state
// while persisting the reviewer's free-text feedback in the SAME statement
// as the status change — `unjira actions decide --edit` needs both to land
// atomically, not as two separate writes a caller could half-apply (e.g. by
// forgetting the second call, or hitting an error between the two).
//
// feedback is written as-is: it is free prose from a human, and quoting or
// escaping it here would just be a second, easier-to-forget place for the
// exact payload-encoding bug class slice 4 already hit once. The driver's
// placeholder binding is what actually protects this from SQL injection or
// truncation, not any transformation of this function's own.
func (s *Store) UpdateActionStatusAndFeedback(id int64, status, feedback string) error {
	return updateActionStatusImpl(s.db, id, status, &feedback)
}

// updateActionStatusImpl backs both UpdateActionStatus and
// UpdateActionStatusAndFeedback so the decided_at/executed_at stamping rules
// live in exactly one place regardless of whether a caller also wants to set
// feedback in the same statement.
func updateActionStatusImpl(c dbConn, id int64, status string, feedback *string) error {
	const ts = `strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`

	query := `UPDATE actions SET status = ?`
	args := []any{status}

	if feedback != nil {
		query += `, feedback = ?`
		args = append(args, *feedback)
	}

	switch status {
	case "approved", "edited", "rejected":
		query += `, decided_at = COALESCE(decided_at, ` + ts + `)`
	case "applied", "failed":
		// An applied action was necessarily decided, but decided_at may
		// already be set from an earlier approval — COALESCE preserves the
		// original ruling time rather than overwriting it.
		query += `, decided_at = COALESCE(decided_at, ` + ts + `), executed_at = ` + ts
	}
	query += ` WHERE id = ?`
	args = append(args, id)

	res, err := c.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("updating action %d to status %q: %w", id, status, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected updating action %d: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("updating action %d to status %q: no such action", id, status)
	}

	return nil
}

// actionSelect is the column list every ActionRow query shares, so a new
// column cannot be added to one query and forgotten in another.
const actionSelect = `SELECT id, narrative_id, type, issue_key, payload, confidence,
	rationale, status, feedback, decided_at, executed_at, created_at FROM actions`

func scanActions(rows *sql.Rows) ([]ActionRow, error) {
	var out []ActionRow

	for rows.Next() {
		var (
			a                             ActionRow
			issueKey, rationale, feedback sql.NullString
			confidence                    sql.NullFloat64
			decidedAt, executedAt         sql.NullString
		)

		if err := rows.Scan(
			&a.ID, &a.NarrativeID, &a.Type, &issueKey, &a.Payload, &confidence,
			&rationale, &a.Status, &feedback, &decidedAt, &executedAt, &a.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning action row: %w", err)
		}

		a.IssueKey = issueKey.String
		a.Rationale = rationale.String
		a.Feedback = feedback.String
		a.Confidence = confidence.Float64
		if decidedAt.Valid {
			a.DecidedAt = &decidedAt.String
		}
		if executedAt.Valid {
			a.ExecutedAt = &executedAt.String
		}

		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating action rows: %w", err)
	}

	return out, nil
}

// -- pipeline lock ---------------------------------------------------------

// lockTimeFormat is the timestamp layout used for pipeline_lock.held_since
// and pipeline_lock.expires_at only — NOT the layout used elsewhere in this
// package. events/narratives/cursors stay on time.RFC3339; switching those
// would mean re-auditing every query that string-compares a timestamp
// (EventsOn, the digest range scans, NarrativeEventsForContext), which is
// why only the lock — where sub-second TTLs are load-bearing — uses this.
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
