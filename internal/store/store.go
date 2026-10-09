// Package store provides SQLite persistence: events, collector cursors, and
// the phase-1+ tables.
//
// The event log is append-only; (source, external_id) is the dedup key so
// collectors can safely re-emit. Every table here has at least one reader and
// one writer: the phase-2 placeholders that did not (estimates, ledger) were
// dropped, because schema is the most-read description of what a system stores
// and a table nothing fills over-states what unjira does.
//
// This package also backs the local tasktracker backend's own mimicked issue
// store (local_issues / local_issue_comments, in localissues.go) — a second,
// unrelated responsibility that happens to share this package's *Store
// handle, Open, and connection pool because both need exactly one SQLite
// file. The two share no query logic: see localissues.go's own doc comment.
//
// Time columns are DATETIME, bound and scanned as time.Time (see timeDSNOptions).
// Three rules keep them comparable. A time is supplied from Go (Store.now), never by
// SQLite, so no statement or default uses strftime. A time is compared only with
// another stored time or a bound time.Time. An aggregate or expression over a time
// column loses its DATETIME type and comes back as text, so read the column itself,
// through a scalar subquery or a window function.
package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

const schema = `
CREATE TABLE IF NOT EXISTS events (
    id           INTEGER PRIMARY KEY,
    source       TEXT NOT NULL,
    external_id  TEXT NOT NULL,
    occurred_at  DATETIME NOT NULL,
    actor        TEXT,
    summary      TEXT NOT NULL,
    artifacts    TEXT NOT NULL DEFAULT '{}',
    raw_ref      TEXT,
    ingested_at  DATETIME NOT NULL,
    UNIQUE (source, external_id)
);
CREATE INDEX IF NOT EXISTS idx_events_occurred ON events (occurred_at);

CREATE TABLE IF NOT EXISTS cursors (
    collector  TEXT NOT NULL,
    resource   TEXT NOT NULL,
    position   TEXT NOT NULL,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (collector, resource)
);

CREATE TABLE IF NOT EXISTS narratives (
    id           INTEGER PRIMARY KEY,
    window_start DATETIME NOT NULL,
    window_end   DATETIME NOT NULL,
    title        TEXT NOT NULL,
    summary      TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'open',
    compaction_boundary DATETIME,
    -- Paired with compaction_boundary to break ties: occurred_at alone
    -- cannot uniquely order events (it is stored at the precision its source
    -- gave, and a second-granular source such as Jira's changelog puts many
    -- events on one instant), so MemberEventsAfterBoundary compares the
    -- (occurred_at, event_id) pair — events.id is a monotonic
    -- INTEGER PRIMARY KEY, an exact tiebreaker for events sharing an instant.
    compaction_boundary_event_id INTEGER,
    created_at   DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS narrative_events (
    -- The link's position in one store-wide sequence, and the ONLY thing that
    -- decides whether a link is newer than an examination or an action (finding
    -- F30). Every such comparison is link_seq against a high-water mark recorded
    -- by linkSeqHighWater: match_examinations.examined_link_seq,
    -- reconcile_examinations.examined_link_seq,
    -- create_examinations.examined_link_seq, actions.created_link_seq (the
    -- reconciler's delta) and actions.executed_link_seq (the freeze rule).
    --
    -- AUTOINCREMENT, not a plain INTEGER PRIMARY KEY, because links are deleted
    -- (restructures unlink; moving a member elsewhere, or upgrading a context link to a
    -- member, deletes the old row) and a
    -- plain rowid reissues a deleted newest number — which would then compare
    -- equal to a high-water mark taken while that row existed.
    --
    -- A member re-link of an event to the narrative it is already a member of keeps
    -- this row and this position (Tx.MoveMember): a re-linked frozen event stays
    -- frozen. A context link UPGRADED to a member is a new row with a new position,
    -- because link_seq means "when the link acquired its current kind" — the
    -- reconciler's delta asks whether this is new WORK, and background that became
    -- work today is new work today.
    link_seq     INTEGER PRIMARY KEY AUTOINCREMENT,
    narrative_id INTEGER NOT NULL REFERENCES narratives (id),
    event_id     INTEGER NOT NULL REFERENCES events (id),
    -- DISPLAY ONLY. Kept because a human-readable time is how most watermark bugs
    -- here have been diagnosed in a sqlite3 session, and it stays useful in ad-hoc
    -- queries. It must NEVER again appear in a > / < comparison that decides
    -- behaviour: it is a wall-clock reading, so two writes can carry the same
    -- instant (when it was written at millisecond resolution, two writes in one
    -- millisecond were byte-identical and a strict > between them was false, which
    -- made every watermark compared on it a tombstone for anything inside one
    -- tick: F30), and a clock can step back. Compare link_seq instead. Like every
    -- stored time it is Store.now bound as a DATETIME (see the package doc), so it sorts
    -- with the other display timestamps when read by eye.
    linked_at    DATETIME NOT NULL,
    -- What this link says about the event (docs/superpowers/specs/2026-10-02-shared-context-design.md
    -- §1). 'member': the event is part of this narrative's WORK — the unit of token attribution, and
    -- the only kind any delta, watermark, matching or drafting path reads. 'context': the event is
    -- relevant background for this narrative and belongs to some other narrative's work.
    --
    -- NOT NULL with NO default, on the precedent actions.created_link_seq set: a default would be a
    -- guess about what the link means, and an INSERT that forgets to say should fail loudly.
    kind         TEXT NOT NULL CHECK (kind IN ('member', 'context')),
    -- The model's stated confidence in a MEMBER placement: its cluster's confidence, or the dispute
    -- re-ask's per-event confidence when two clusters claimed the event. A context link carries
    -- none, because it attributes nothing. Read against correlator.member_confidence_floor, which
    -- surfaces a below-floor attribution in triage.
    --
    -- The IS NOT NULL is not redundant with BETWEEN: a CHECK passes when its expression
    -- is NULL, and NULL BETWEEN 0 AND 1 is NULL, so without it a member with no
    -- confidence would be accepted.
    member_confidence REAL CHECK (
        (kind = 'member' AND member_confidence IS NOT NULL AND member_confidence BETWEEN 0 AND 1)
        OR (kind = 'context' AND member_confidence IS NULL)),
    -- WHO placed a MEMBER link (MemberPlacement): 'model' (a clustering or split answer, at the
    -- model's stated confidence), 'identity' (the clustering pre-filter joined the event to the
    -- one open narrative already holding its exact pull request, never shown to the model), or
    -- 'reviewer' (triage's merge). member_confidence alone cannot say: identity and reviewer both
    -- record 1.0, and so can the model. A reader calibrating the model's confidence against
    -- reviewer rulings must count only 'model'. A context link places no member and carries none.
    --
    -- Same CHECK shape as member_confidence, and the IS NOT NULL for the same reason: NULL IN (...)
    -- is NULL, which a CHECK passes (design-notes #44).
    member_placement TEXT CHECK (
        (kind = 'member' AND member_placement IS NOT NULL AND member_placement IN ('model', 'identity', 'reviewer'))
        OR (kind = 'context' AND member_placement IS NULL)),
    UNIQUE (narrative_id, event_id)
);

-- Every linked event has AT MOST one member home, enforced by the database rather than by
-- convention — the same shape as one_primary_per_narrative below. "Whose work is this event"
-- must have one answer, because member links are what a reconcile pass drafts from and what
-- token attribution charges. AT LEAST one is the other half of the invariant, checked when
-- correlator.Persist commits (an event given a context link must hold a member link when the
-- transaction closes), because a partial index cannot express "exists".
CREATE UNIQUE INDEX IF NOT EXISTS one_member_link_per_event
    ON narrative_events (event_id) WHERE kind = 'member';

CREATE TABLE IF NOT EXISTS narrative_issues (
    narrative_id INTEGER NOT NULL REFERENCES narratives (id),
    issue_key    TEXT    NOT NULL,
    role         TEXT    NOT NULL,   -- primary | same_work | mentioned
    provenance   TEXT    NOT NULL,   -- reviewer | branch | jira_event | corroborated | prose_first | prose_later
    confidence   REAL,
    connection   TEXT,               -- which connection resolved it
    created_at   DATETIME NOT NULL,
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
    decided_at   DATETIME,
    -- executed_at is DISPLAY ONLY, like created_at below: the freeze rule reads
    -- executed_link_seq, never this. See narrative_events.linked_at for why a
    -- wall-clock timestamp must not decide behaviour.
    executed_at  DATETIME,
    -- The link high-water mark when this action was last executed (applied or
    -- failed), stamped in the same statement as executed_at. The freeze rule —
    -- "a link made before the narrative's last APPLIED action is frozen" — is
    -- narrative_events.link_seq > MAX(executed_link_seq). NULL until executed.
    executed_link_seq INTEGER,
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
    -- DISPLAY and ORDERING only — ORDER BY created_at, id is safe because id
    -- breaks the tie. It must never again be compared with > / < against
    -- narrative_events.linked_at to decide what is delta; created_link_seq does
    -- that. Stored like linked_at, so the two still read side by side.
    created_at   DATETIME NOT NULL,
    -- The link high-water mark when this action was created. The reconciler's
    -- delta (DeltaEvents, hasUnexaminedDelta) is narrative_events.link_seq >
    -- MAX(created_link_seq) over the narrative's actions of any status. NOT NULL
    -- with no DEFAULT on purpose: a default would be a guess about which links
    -- predate the action, and an INSERT that forgets it should fail loudly.
    created_link_seq INTEGER NOT NULL,
    -- The correction sequence (correction_marks.seq) when this action last
    -- BECAME a correction: ruled rejected or edited with non-empty feedback, or
    -- had that feedback changed. The learn cursor reads corrections with
    -- corrected_seq above it, never decided_at (findings F41, F42). decided_at
    -- keeps the first ruling's time, so a lesson added by a later ruling sat
    -- below a cursor that had already passed it; and decided_at is the wall
    -- clock, which can step back. NULL until the action is first a correction.
    corrected_seq INTEGER,
    -- Every correction carries its sequence. A write path that made a row a
    -- correction without stamping one would hide it from every learn pass,
    -- so the schema refuses it rather than leaving it unseen.
    CHECK (status NOT IN ('rejected', 'edited')
           OR TRIM(COALESCE(feedback, '')) = ''
           OR corrected_seq IS NOT NULL)
);

-- correction_marks issues the correction sequence: one row each time an action
-- becomes a correction (updateActionStatusImpl), never updated or deleted. Its
-- AUTOINCREMENT seq is why actions.corrected_seq can be trusted to grow: SQLite
-- never reissues an AUTOINCREMENT value, even after a delete, the property
-- narrative_events.link_seq relies on too (linkSeqHighWater). It also records
-- how an action's lessons were revised, though nothing reads that yet.
CREATE TABLE IF NOT EXISTS correction_marks (
    seq       INTEGER PRIMARY KEY AUTOINCREMENT,
    action_id INTEGER NOT NULL REFERENCES actions(id),
    -- DISPLAY only, like every other timestamp beside a sequence.
    marked_at DATETIME NOT NULL
);

-- The estimates and ledger tables were declared here as phase-2 placeholders, with no Go
-- code reading or writing either (finding F5). Dropped rather than kept: schema is the
-- most-read description of what a system stores, so a newcomer counting tables
-- over-counted what unjira actually does. git history holds their shape for whenever
-- phase 2 needs it, and re-adding a CREATE TABLE is cheaper than leaving a reader to
-- trust a table nothing fills.
--
-- Existing databases keep their now-orphaned tables — this package has no migration
-- mechanism (every statement is CREATE TABLE IF NOT EXISTS), so removal applies to new
-- stores only. Harmless: nothing referenced them.

CREATE TABLE IF NOT EXISTS pipeline_lock (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    run_id     TEXT NOT NULL,
    held_since DATETIME NOT NULL,
    expires_at DATETIME NOT NULL
);
`

// Store wraps a SQLite connection with unjira's schema and access methods.
type Store struct {
	db *sql.DB
	// log is optional. Nil means silent, so every existing caller of Open keeps working
	// and no accessor needs a nil check — logging.For handles that. Set with SetLogger
	// after Open rather than as an Open parameter, because Open's signature is used in
	// dozens of tests that have no interest in logs.
	log *slog.Logger
	// now is the store's clock: every time the store stamps itself (ingested_at,
	// linked_at, created_at, decided_at, examined_at and the rest) is now(), bound as a
	// time.Time like every other stored time (see the package doc), never SQLite's
	// strftime('now'), whose text has a different shape. time.Now from Open.
	now func() time.Time
}

// SetLogger attaches a logger to an already-open store.
//
// Deliberately not an Open parameter: Open is called from many tests and from every
// command, and threading a logger through all of them to serve two log lines would be
// noise. The store is constructed once per process in cmd/unjira's run(), which is
// exactly where the logger already exists.
func (s *Store) SetLogger(log *slog.Logger) {
	s.log = log
}

// SetClock replaces the store's clock, the source of every time the store stamps
// itself (ingested_at, updated_at, linked_at, created_at and the rest). Open sets
// time.Now. Not an Open parameter, for SetLogger's reason; its callers are tests that
// need a stamp to be a chosen instant (internal/store/storetest).
func (s *Store) SetClock(now func() time.Time) {
	s.now = now
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
	// now is the Store's clock, for the stamps a write through this Tx makes.
	now func() time.Time
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

	if err := fn(&Tx{tx: tx, now: s.now}); err != nil {
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

	// Checked BEFORE any schema statement, never after. CREATE TABLE IF NOT EXISTS
	// leaves an older database's existing tables as they were — so a store created
	// before the link sequence (F30) keeps its old columns — but it also CREATES
	// whichever tables that store never had, in the new shape. Checking afterwards
	// therefore refused the store only after already mutating it: a real pre-F30
	// store came back from a refused open with a new-shaped reconcile_examinations,
	// which the old build cannot use, breaking the README's "run learn on the old
	// build first" escape hatch.
	format, hasTables, err := readStoreFormat(db, dbPath)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := checkRequiredColumns(db, dbPath, format, hasTables); err != nil {
		_ = db.Close()
		return nil, err
	}

	// A store this build creates is stamped with its format before its first table, so
	// a schema statement failing part-way leaves a store the next open completes rather
	// than one it refuses as old.
	if !hasTables {
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, storeFormat)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("recording the store format of %s: %w", dbPath, err)
		}
	}

	if _, err := db.Exec(schema + localIssuesSchema + matchExaminationsSchema + reconcileExaminationsSchema +
		createExaminationsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("applying schema to %s: %w", dbPath, err)
	}

	return &Store{db: db, now: time.Now}, nil
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

	return "file:" + p + "?_pragma=foreign_keys(1)&" + timeDSNOptions
}

// timeDSNOptions makes the driver render every bound time.Time in UTC at full
// precision, e.g. "2026-10-07 03:37:37.998156484+00:00", and scan it back in UTC.
// Text order is still instant order: trailing fractional zeros are trimmed, but
// every value ends in "+00:00" and '+' sorts below '.' and every digit (pinned by
// TestStoredTimes_TextOrderIsInstantOrder).
const timeDSNOptions = "_time_format=sqlite&_timezone=UTC"

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
