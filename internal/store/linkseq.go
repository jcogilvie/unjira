package store

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
// Tx.MoveMember), and a plain INTEGER PRIMARY KEY would hand the
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
