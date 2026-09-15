// Package store provides SQLite persistence: events, collector cursors, and
// the phase-1+ tables.
//
// The event log is append-only; (source, external_id) is the dedup key so
// collectors can safely re-emit. Narratives/actions/estimates/ledger are
// created now so the schema is stable, but only events and cursors are
// written in phase 0.
//
// This package also backs the local tasktracker backend's own mimicked issue
// store (local_issues / local_issue_comments, in localissues.go) — a second,
// unrelated responsibility that happens to share this package's *Store
// handle, Open, and connection pool because both need exactly one SQLite
// file. The two share no query logic: see localissues.go's own doc comment.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

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
    provenance   TEXT    NOT NULL,   -- reviewer | branch | jira_event | corroborated | prose_first | prose_later
    confidence   REAL,
    connection   TEXT,               -- which JiraConnection resolved it
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (narrative_id, issue_key)
);

-- Exactly one primary per narrative, enforced by the database rather than by
-- convention. "Which issue is this narrative's" must have one answer: this row
-- IS that answer (a narratives.issue_key column used to denormalize it, and
-- drifted -- see F11), and NarrativesWithoutPrimaryLink treats the presence of
-- this row as "attributed". Two primaries would make both questions arbitrary.
-- The composite PRIMARY KEY above prevents duplicate keys per narrative but
-- permits two rows both marked primary, which is the case this closes.
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
                                      -- | suppressed  (a DETERMINISTIC filter refused
                                      --   the draft; the row is a watermark so the
                                      --   same suppression is not re-derived forever)
                                      -- | declined  (the MODEL judged the work not
                                      --   worth a ticket; no human ruled, nothing
                                      --   was written. Distinct from 'rejected',
                                      --   which is a human's ruling — see
                                      --   reconciler.StatusDeclined)
    decided_at   TEXT,
    executed_at  TEXT,
    -- Written by slice 6's triage/rework loop, nothing today. Added now so
    -- that slice does not need a second schema change (see the phase-1 spec's
    -- schema-additions section, where it was specified but never landed).
    feedback     TEXT,
    -- Set by gate.Applier.Apply when status transitions to 'failed': the
    -- tracker error's message (e.g. "posting comment to PAAS-1: 403
    -- Forbidden"), so a human can see WHY without re-running the write.
    -- Distinct from feedback above and deliberately kept in its own column:
    -- feedback is a human reviewer's free-text correction, read by triage's
    -- rework loop and rules.Distill (slice 7) as if a person wrote it. A
    -- machine-written tracker error landing in that column would corrupt rule
    -- distillation with 403s. Cleared (set back to '') on a subsequent
    -- successful apply, so a retried-and-fixed action never reports a stale
    -- reason.
    error        TEXT,
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
//
// The schema applied here is both this package's own tables (schema, above)
// and the local tasktracker backend's mimicked-issue tables
// (localIssuesSchema, in localissues.go) — in one Exec call, so table
// creation stays a single atomic statement sequence regardless of which
// backend config.json actually names. Open takes no backend parameter: the
// local_issues/local_issue_comments tables are created unconditionally, even
// when the jira backend is configured (the default). That is a known
// property, not an oversight of this move — see docs/architecture-findings.md
// F3 (which now covers only the correlator's Jira dependency; whether
// local_issues creation should be conditional on the backend is a separate,
// open design question this split does not take a position on).
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

	if _, err := db.Exec(schema + localIssuesSchema); err != nil {
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
