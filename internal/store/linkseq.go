package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// linkSeqHighWater is the narrative_events link sequence's high-water mark: the
// largest link_seq ever issued, or 0 before the first link. Every comparison that
// decides whether a link is "newer than" an examination or an action compares
// narrative_events.link_seq against a value this expression recorded (finding F30).
//
// WHY A SEQUENCE AND NOT THE TIMESTAMPS. Those comparisons used to be
// `linked_at > examined_at` (or created_at, or executed_at), all written as
// strftime('%Y-%m-%dT%H:%M:%fZ','now'). Two `now` calls inside one millisecond are
// byte-identical, so a link made in the same millisecond as the examination it
// should have re-opened was never "newer": the watermark was a tombstone for
// anything inside one tick. It surfaced as a one-in-thirty test flake, but the
// production path had the same hazard — a fast collect -> match on a small store.
// No timestamp format fixes that; the clock's resolution is the bug. A sequence has
// no resolution.
//
// WHY sqlite_sequence AND NOT MAX(link_seq). link_seq is AUTOINCREMENT precisely so
// a number is never reissued — restructures DELETE links (UnlinkNarrativeEvents,
// UnlinkEventFromOtherNarratives), and a plain INTEGER PRIMARY KEY would hand the
// deleted newest number to the next link, which would then compare equal to a
// high-water mark recorded while the deleted row existed. sqlite_sequence is where
// SQLite keeps AUTOINCREMENT's "largest ever issued", so it is monotonic by
// construction: every link inserted after this expression is evaluated gets a
// strictly greater link_seq, and every link that existed when it was evaluated has a
// link_seq at or below it. That pair of facts is the whole contract.
//
// The row is absent until narrative_events' first insert, hence the COALESCE: 0 is
// below every link_seq, so "recorded before any link existed" means every link is
// newer. Parenthesized so it drops into a VALUES list or a SET clause unchanged, and
// carries no bound parameters, so interpolating it disturbs no query's argument order.
const linkSeqHighWater = `(SELECT COALESCE(
	(SELECT seq FROM sqlite_sequence WHERE name = 'narrative_events'), 0))`

// linkSeqColumns are the columns F30's comparisons depend on, by table.
//
// Open checks them because this package has no migration mechanism: every schema
// statement is CREATE TABLE IF NOT EXISTS, so a database created before F30 keeps
// its old tables and silently lacks every one of these. The queries that reference
// them would then fail mid-pass with SQLite's "no such column", which names neither
// the cause nor the fix.
var linkSeqColumns = []struct{ table, column string }{
	{"narrative_events", "link_seq"},
	{"actions", "created_link_seq"},
	{"actions", "executed_link_seq"},
	{"match_examinations", "examined_link_seq"},
	{"reconcile_examinations", "examined_link_seq"},
}

// checkLinkSeqSchema refuses a database that predates the link sequence, naming
// every missing column and the fix.
//
// Refusing is the only safe outcome. Adding the columns with ALTER TABLE could not
// give pre-existing links a meaningful position — their order relative to existing
// examinations and actions is exactly what the old timestamps could not establish —
// and the store is disposable pre-release, so re-collecting is cheap where a guessed
// backfill would be silently wrong.
func checkLinkSeqSchema(db *sql.DB, dbPath string) error {
	var missing []string

	for _, c := range linkSeqColumns {
		// Runs before the schema is applied, so a table that does not exist yet is
		// not "missing a column": this build is about to create it in the new shape.
		// Only a table that already exists without the column marks an old store.
		var exists int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, c.table,
		).Scan(&exists); err != nil {
			return fmt.Errorf("checking %s for table %s: %w", dbPath, c.table, err)
		}
		if exists == 0 {
			continue
		}

		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column,
		).Scan(&n); err != nil {
			return fmt.Errorf("checking %s for column %s.%s: %w", dbPath, c.table, c.column, err)
		}
		if n == 0 {
			missing = append(missing, c.table+"."+c.column)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf(
			"database %s predates the link sequence (finding F30): missing %s. This build "+
				"orders narrative_events links by sequence rather than by timestamp, and the "+
				"store has no migrations, so this database cannot be upgraded in place: "+
				"delete the database and re-collect",
			dbPath, strings.Join(missing, ", "))
	}

	return nil
}
