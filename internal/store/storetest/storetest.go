// Package storetest builds store fixtures for tests: fluent builders with defaults for
// events, narratives and cursors, a declarative Fixture that Seed writes and answers
// with a name-to-id map, time constructors that read as values, and a raw store file
// writer for Open's cases. Import it only from _test.go files.
//
// Every fixture names its rows. An event's name is its external id and a narrative's
// is its title, so a test states what it expects by name and never by database id.
package storetest

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "modernc.org/sqlite" // WriteStoreFile and Tables open the file directly

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// -- times -------------------------------------------------------------------------

// PDT is the offset Jira reports for a US-Pacific account in summer. Within one offset
// text order and instant order agree, so a fixture shows an offset bug only when it
// mixes PDT with UTC.
var PDT = time.FixedZone("PDT", -7*60*60)

// UTC is 2026-10-06 at the given UTC time. nsec is optional, for a case that needs a
// fraction of a second.
func UTC(hour, minute, sec int, nsec ...int) time.Time {
	return on(time.UTC, hour, minute, sec, nsec)
}

// Pacific is 2026-10-06 at the given wall-clock time in PDT, seven hours behind UTC.
func Pacific(hour, minute, sec int, nsec ...int) time.Time {
	return on(PDT, hour, minute, sec, nsec)
}

// Midnight is the start of 2026-10-<dayOfMonth> in UTC, the day EventsOn reads.
func Midnight(dayOfMonth int) time.Time {
	return time.Date(2026, 10, dayOfMonth, 0, 0, 0, 0, time.UTC)
}

func on(zone *time.Location, hour, minute, sec int, nsec []int) time.Time {
	ns := 0
	if len(nsec) > 0 {
		ns = nsec[0]
	}

	return time.Date(2026, 10, 6, hour, minute, sec, ns, zone)
}

// -- events ------------------------------------------------------------------------

// EventBuilder builds one events.Event. Defaults: source claude_code, at UTC(12, 0, 0),
// summary the name, no artifacts.
type EventBuilder struct {
	source   string
	name     string
	at       time.Time
	issueKey string
	statusTo string
}

// Event starts an event whose external id is name.
func Event(name string) *EventBuilder {
	return &EventBuilder{source: "claude_code", name: name, at: UTC(12, 0, 0)}
}

// Source sets the collector source.
func (b *EventBuilder) Source(source string) *EventBuilder {
	b.source = source

	return b
}

// At sets when the event occurred.
func (b *EventBuilder) At(at time.Time) *EventBuilder {
	b.at = at

	return b
}

// Issue sets the issue the event concerns (events.ArtifactIssueKey).
func (b *EventBuilder) Issue(key string) *EventBuilder {
	b.issueKey = key

	return b
}

// StatusTo makes the event a status change to the named status (events.ArtifactStatusTo).
func (b *EventBuilder) StatusTo(status string) *EventBuilder {
	b.statusTo = status

	return b
}

// Build returns the event.
func (b *EventBuilder) Build() events.Event {
	e := events.NewEvent(b.source, b.name, b.at, b.name)
	if b.issueKey != "" {
		e.Artifacts[events.ArtifactIssueKey] = b.issueKey
	}
	if b.statusTo != "" {
		e.Artifacts[events.ArtifactStatusTo] = b.statusTo
	}

	return e
}

// -- narratives --------------------------------------------------------------------

// NarrativeSpec is one narrative a Fixture seeds.
type NarrativeSpec struct {
	Title      string
	Start, End time.Time
	// Members names the fixture's events linked to this narrative as members.
	Members []string
	// Compacted is the compaction boundary, nil for none.
	Compacted *Compaction
}

// Compaction is a compaction boundary: an event, by name, and the boundary time.
type Compaction struct {
	Event string
	At    time.Time
}

// NarrativeBuilder builds a NarrativeSpec. Defaults: window [UTC(9, 0, 0),
// UTC(10, 0, 0)], no members, never compacted.
type NarrativeBuilder struct {
	spec NarrativeSpec
}

// Narrative starts a narrative titled title.
func Narrative(title string) *NarrativeBuilder {
	return &NarrativeBuilder{spec: NarrativeSpec{Title: title, Start: UTC(9, 0, 0), End: UTC(10, 0, 0)}}
}

// Window sets the narrative's window.
func (b *NarrativeBuilder) Window(start, end time.Time) *NarrativeBuilder {
	b.spec.Start, b.spec.End = start, end

	return b
}

// Members links the named events to the narrative as members.
func (b *NarrativeBuilder) Members(events ...string) *NarrativeBuilder {
	b.spec.Members = append(b.spec.Members, events...)

	return b
}

// CompactedAt sets the compaction boundary to the named event, at the given time.
func (b *NarrativeBuilder) CompactedAt(event string, at time.Time) *NarrativeBuilder {
	b.spec.Compacted = &Compaction{Event: event, At: at}

	return b
}

// Build returns the spec.
func (b *NarrativeBuilder) Build() NarrativeSpec {
	return b.spec
}

// -- cursors -----------------------------------------------------------------------

// CursorSpec is one collector cursor a Fixture seeds, stamped at UpdatedAt.
type CursorSpec struct {
	Collector, Resource, Position string
	UpdatedAt                     time.Time
}

// CursorBuilder builds a CursorSpec. Defaults: collector claude_code, position "1",
// updated at UTC(12, 0, 0).
type CursorBuilder struct {
	spec CursorSpec
}

// Cursor starts a cursor on resource.
func Cursor(resource string) *CursorBuilder {
	return &CursorBuilder{spec: CursorSpec{
		Collector: "claude_code", Resource: resource, Position: "1", UpdatedAt: UTC(12, 0, 0),
	}}
}

// Collector sets the collector.
func (b *CursorBuilder) Collector(collector string) *CursorBuilder {
	b.spec.Collector = collector

	return b
}

// UpdatedAt sets when the cursor was written.
func (b *CursorBuilder) UpdatedAt(at time.Time) *CursorBuilder {
	b.spec.UpdatedAt = at

	return b
}

// Build returns the spec.
func (b *CursorBuilder) Build() CursorSpec {
	return b.spec
}

// -- seeding -----------------------------------------------------------------------

// Fixture is a store's contents, declared. Names must be unique across events and
// narratives, since Seed answers with one map.
type Fixture struct {
	Events     []events.Event
	Narratives []NarrativeSpec
	Cursors    []CursorSpec
}

// IDs maps each seeded event's and narrative's name to its row id.
type IDs map[string]int64

// Seed writes f to s: events, then narratives with their member links and compaction
// boundaries, then cursors, each stamped at its UpdatedAt through Store.SetClock. It
// leaves the store's clock at time.Now.
func Seed(tb testing.TB, s *store.Store, f Fixture) IDs {
	tb.Helper()

	ids := make(IDs)
	name := func(n string, id int64) {
		tb.Helper()
		if _, dup := ids[n]; dup {
			tb.Fatalf("storetest: the name %q is used twice in one fixture", n)
		}
		ids[n] = id
	}

	for _, e := range f.Events {
		if _, err := s.InsertEvent(e); err != nil {
			tb.Fatalf("storetest: seeding event %q: %v", e.ExternalID, err)
		}
		id, err := s.EventIDByExternalID(e.Source, e.ExternalID)
		if err != nil {
			tb.Fatalf("storetest: reading event %q's id: %v", e.ExternalID, err)
		}
		name(e.ExternalID, id)
	}

	for _, n := range f.Narratives {
		id, err := s.InsertNarrative(n.Start, n.End, n.Title, "summary")
		if err != nil {
			tb.Fatalf("storetest: seeding narrative %q: %v", n.Title, err)
		}
		name(n.Title, id)

		if err := s.LinkMembers(id, eventIDs(tb, ids, n.Members), 1); err != nil {
			tb.Fatalf("storetest: linking narrative %q's members: %v", n.Title, err)
		}
		if c := n.Compacted; c != nil {
			if err := s.SetCompactionBoundary(id, c.At, eventIDs(tb, ids, []string{c.Event})[0], "recap"); err != nil {
				tb.Fatalf("storetest: compacting narrative %q: %v", n.Title, err)
			}
		}
	}

	for _, c := range f.Cursors {
		s.SetClock(func() time.Time { return c.UpdatedAt })
		if err := s.SetCursor(c.Collector, c.Resource, c.Position); err != nil {
			tb.Fatalf("storetest: seeding cursor %s/%s: %v", c.Collector, c.Resource, err)
		}
	}
	s.SetClock(time.Now)

	return ids
}

func eventIDs(tb testing.TB, ids IDs, names []string) []int64 {
	tb.Helper()

	out := make([]int64, 0, len(names))
	for _, n := range names {
		id, ok := ids[n]
		if !ok {
			tb.Fatalf("storetest: no event named %q in the fixture", n)
		}
		out = append(out, id)
	}

	return out
}

// EventNames is each event's name (its external id), in order.
func EventNames(evs []events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.ExternalID)
	}

	return out
}

// NarrativeNames is each narrative's name (its title), in order.
func NarrativeNames(rows []store.NarrativeRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}

	return out
}

// -- raw store files ---------------------------------------------------------------

// StoreFile describes a database file as some build may have left it, for Open's cases.
type StoreFile struct {
	// Format is the store format recorded in PRAGMA user_version.
	Format int
	// Tables is whether the file holds this build's tables; false leaves it empty.
	Tables bool
	// Without names tables to drop afterwards, so a test can see whether Open
	// recreates them.
	Without []string
}

// WriteStoreFile writes f at path.
func WriteStoreFile(tb testing.TB, path string, f StoreFile) {
	tb.Helper()

	if f.Tables {
		s, err := store.Open(path)
		if err != nil {
			tb.Fatalf("storetest: creating the store at %s: %v", path, err)
		}
		if err := s.Close(); err != nil {
			tb.Fatalf("storetest: closing the store at %s: %v", path, err)
		}
	}

	db := openRaw(tb, path)
	defer func() { _ = db.Close() }()

	stmts := make([]string, 0, 1+len(f.Without))
	stmts = append(stmts, fmt.Sprintf(`PRAGMA user_version = %d`, f.Format))
	for _, table := range f.Without {
		stmts = append(stmts, fmt.Sprintf(`DROP TABLE %q`, table))
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			tb.Fatalf("storetest: %s on %s: %v", stmt, path, err)
		}
	}
}

// Tables lists the tables in the database file at path, sorted, read without Open.
func Tables(tb testing.TB, path string) []string {
	tb.Helper()

	db := openRaw(tb, path)
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		tb.Fatalf("storetest: listing the tables of %s: %v", path, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			tb.Fatalf("storetest: scanning a table name of %s: %v", path, err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		tb.Fatalf("storetest: listing the tables of %s: %v", path, err)
	}

	return out
}

func openRaw(tb testing.TB, path string) *sql.DB {
	tb.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		tb.Fatalf("storetest: opening %s: %v", path, err)
	}
	if err := db.Ping(); err != nil {
		tb.Fatalf("storetest: opening %s: %v", path, err)
	}

	return db
}
