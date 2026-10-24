package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// requiredColumn is one column a query in this build depends on, and the change that
// introduced it — named in the refusal so an operator can tell which change made their
// store old.
type requiredColumn struct {
	table, column string
	change        string
}

// The changes requiredColumns names. Constants so the refusal groups columns by change
// rather than repeating a sentence per column.
const (
	changeLinkSeq = "the link sequence (finding F30), which orders narrative_events links by " +
		"sequence rather than by timestamp"
	changeLinkKinds = "member and context links (docs/superpowers/specs/2026-10-02-shared-context-design.md), " +
		"which give every narrative_events link a kind and every member link a confidence"
	changeCorrectionSeq = "the correction sequence (findings F41 and F42), which orders reviewer corrections " +
		"by when each became a correction rather than by the first ruling's clock time"
	changeMemberPlacement = "member placement (finding F43's pull-request identity join), which records " +
		"whether the model, an exact pull-request identity or a reviewer placed each member link"
	changeUTCTimestamps = "UTC timestamps (finding F53), which store every timestamp as a DATETIME in UTC at full " +
		"precision, so that comparing two as text orders them by instant"
	changeTokenLedger = "the token ledger (docs/superpowers/specs/2026-10-14-token-attribution-design.md), which " +
		"records each Claude Code assistant message's token usage as its transcript is read; a store collected " +
		"before it has no ledger rows for any transcript it has already read, and never re-reads them"
	changeWorklogSink = "the worklog sink (docs/superpowers/specs/2026-10-14-token-attribution-design.md), which " +
		"records on each ledger message the Jira worklog it was logged in; a store from before it has no such " +
		"record, so every message it already logged would be proposed again"
)

// storeFormat is the store format this build writes, recorded in SQLite's
// user_version header field (PRAGMA user_version) when Open creates a store.
//
// It marks changes no column name can: F53 changed how existing time columns are
// declared and what their values mean (DATETIME in UTC rather than TEXT in the source's
// offset), not which columns exist, so a store written before it has every column
// requiredColumns names. Sniffing the data instead would mean reading every
// timestamp column for its shape on each open, and would pass an old store whose
// timestamps happened all to be UTC, though nothing
// promised they would stay so. The marker is set once, on a store with no tables, and
// never rewritten, so a build cannot lower the format of a store a newer build made.
//
// Formats: 0 (SQLite's default, so every store made before the marker existed), in
// which a timestamp keeps its source's offset; 1, in which every timestamp written
// is a DATETIME bound as a time.Time, which the driver stores in UTC at full precision
// (see the package doc); 2, which adds the token_messages and llm_calls tables. A new
// table trips no column check (columnMissing passes a table that does not exist), so
// without the marker an older store would open with empty ledger tables, and the
// transcripts it has already read past would never be re-read: their spend would be
// silently absent; 3, which adds token_messages.worklog_id and worklog_issue_key and
// the attribution_sink_state table, without which every message an older store already
// logged would be proposed again.
const storeFormat = len(formatChanges) - 1

// formatChanges names the change each store format introduced, indexed by format (0
// introduced nothing). An array, and storeFormat is derived from its length, so a
// format without a named change cannot be written. A store whose format is older is
// refused naming every change newer than it.
var formatChanges = [...]string{
	1: changeUTCTimestamps,
	2: changeTokenLedger,
	3: changeWorklogSink,
}

// requiredColumns are the columns this build's queries read that an older store can lack.
//
// Open checks them because this package has no migration mechanism: every schema
// statement is CREATE TABLE IF NOT EXISTS, so a database created before a change keeps
// its old tables and silently lacks the change's columns. The queries that reference
// them would then fail mid-pass with SQLite's "no such column", which names neither the
// cause nor the fix — or, worse, a query that does not reference a column would run with
// the old semantics. Every link-reading query now filters on narrative_events.kind, so a
// store without it is refused rather than read as if every link were a member.
var requiredColumns = []requiredColumn{
	{tableNarrativeEvents, "link_seq", changeLinkSeq},
	{tableActions, "created_link_seq", changeLinkSeq},
	{tableActions, "executed_link_seq", changeLinkSeq},
	{"match_examinations", "examined_link_seq", changeLinkSeq},
	{"reconcile_examinations", "examined_link_seq", changeLinkSeq},
	{tableNarrativeEvents, "kind", changeLinkKinds},
	{tableNarrativeEvents, "member_confidence", changeLinkKinds},
	{tableNarrativeEvents, "member_placement", changeMemberPlacement},
	{tableActions, "corrected_seq", changeCorrectionSeq},
	{"token_messages", "worklog_id", changeWorklogSink},
	{"token_messages", "worklog_issue_key", changeWorklogSink},
}

const (
	tableNarrativeEvents = "narrative_events"
	tableActions         = "actions"
)

// checkRequiredColumns refuses a database that predates any change in requiredColumns,
// or whose recorded format (read by readStoreFormat) is older than storeFormat, naming
// every missing column, the change that introduced it, and the fix. A database with no
// tables (hasTables false) is a store this build is about to create, never an old one.
//
// Refusing is the only safe outcome, and there is deliberately no ALTER-based upgrade:
// unjira has no migrations until it is productionized (decided 2026-10-02;
// the shared-context spec's open question 9). Each change here has its own reason a
// backfill would be wrong or premature. F30's: ALTER TABLE could not give pre-existing
// links a meaningful position, since their order relative to existing examinations and
// actions is exactly what the old timestamps could not establish. The link kinds': that
// backfill would be exact (every older link is a member, one per event), but it would be
// unjira's first migration, a precedent to set deliberately for a store someone cannot
// afford to lose, not inside a feature change against a disposable one. Member
// placement's: a backfill would be a guess, since an older member link may have been
// placed by the model or by a reviewer's merge, and the store kept no record of which.
// The correction sequence's: it records when each action became a correction, and the
// store kept no record of that either, only the first ruling's clock time, which is the
// value F42 found wrong. UTC timestamps': converting an old store's offset timestamps
// would be exact, but it would be a migration all the same. The token ledger's: its rows
// come from transcript lines the old store's cursors have already passed, so only a
// re-collect can produce them.
//
// MUST run before any schema statement: see Open.
func checkRequiredColumns(db *sql.DB, dbPath string, format int, hasTables bool) error {
	missingByChange := make(map[string][]string)
	var changeOrder []string

	for _, c := range requiredColumns {
		missing, err := columnMissing(db, dbPath, c.table, c.column)
		if err != nil {
			return err
		}
		if !missing {
			continue
		}

		if _, seen := missingByChange[c.change]; !seen {
			changeOrder = append(changeOrder, c.change)
		}
		missingByChange[c.change] = append(missingByChange[c.change], c.table+"."+c.column)
	}

	parts := make([]string, 0, len(changeOrder)+1)
	for _, change := range changeOrder {
		parts = append(parts, fmt.Sprintf("%s (missing %s)", change, strings.Join(missingByChange[change], ", ")))
	}
	if hasTables {
		// A negative user_version (the header field is signed) is no format this build
		// wrote; read it as 0, so it is refused naming every change.
		for f := max(format, 0) + 1; f <= storeFormat; f++ {
			parts = append(parts, fmt.Sprintf("%s (store format %d; this build requires %d)",
				formatChanges[f], format, storeFormat))
		}
	}

	if len(parts) == 0 {
		return nil
	}

	return fmt.Errorf(
		"database %s predates %s. The store has no migrations, so this database cannot be "+
			"upgraded in place: rename the database to a backup (e.g. mv %s %s.bak) and re-collect",
		dbPath, strings.Join(parts, "; and "), dbPath, dbPath)
}

// columnMissing reports whether table exists WITHOUT column.
//
// Runs before the schema is applied, so a table that does not exist yet is not "missing
// a column": this build is about to create it in the new shape. Only a table that
// already exists without the column marks an old store.
func columnMissing(db *sql.DB, dbPath, table, column string) (bool, error) {
	var exists int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("checking %s for table %s: %w", dbPath, table, err)
	}
	if exists == 0 {
		return false, nil
	}

	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("checking %s for column %s.%s: %w", dbPath, table, column, err)
	}

	return n == 0, nil
}

// readStoreFormat returns the database's recorded store format (see storeFormat) and
// whether it holds any table at all. Read-only, so it is safe before the schema check.
func readStoreFormat(db *sql.DB, dbPath string) (format int, hasTables bool, err error) {
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&format); err != nil {
		return 0, false, fmt.Errorf("reading the store format of %s: %w", dbPath, err)
	}

	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
		return 0, false, fmt.Errorf("checking %s for tables: %w", dbPath, err)
	}

	return format, tables > 0, nil
}
