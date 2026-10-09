package store_test

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/store/storetest"
)

// Stored times are DATETIMEs the driver renders in UTC at full precision. The fixtures
// here mix PDT with UTC on purpose: within one offset, text order and instant order
// agree, so a single-offset fixture would pass whatever the store wrote. Each table
// tests one function: its runner seeds the row's fixture, calls the function once, and
// compares names.

// -- window queries ----------------------------------------------------------------

func TestUnlinkedEventsInRange_ComparesOffsetsByInstant(t *testing.T) {
	tests := []struct {
		name       string
		fixture    storetest.Fixture
		start, end time.Time
		want       []string
	}{
		{
			name: "stored offsets are ordered and bounded by instant",
			fixture: storetest.Fixture{Events: []events.Event{
				// 13:37:35 PDT is 20:37:35Z: inside the window, though its text sorts before it.
				storetest.Event("jira").At(storetest.Pacific(13, 37, 35)).Build(),
				// Before the window, though its text sorts after the Jira event's.
				storetest.Event("early").At(storetest.UTC(18, 0, 0)).Build(),
				storetest.Event("late").At(storetest.UTC(20, 50, 0)).Build(),
			}},
			start: storetest.UTC(18, 30, 0),
			end:   storetest.UTC(21, 0, 0),
			want:  []string{"jira", "late"},
		},
		{
			name: "offset bounds are compared by instant",
			fixture: storetest.Fixture{Events: []events.Event{
				storetest.Event("in").At(storetest.UTC(20, 37, 35)).Build(),
				// Before the window, though its text sorts after the start bound's local text.
				storetest.Event("before").At(storetest.UTC(19, 30, 0)).Build(),
			}},
			start: storetest.Pacific(13, 0, 0), // 20:00Z
			end:   storetest.Pacific(14, 0, 0), // 21:00Z
			want:  []string{"in"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			storetest.Seed(t, s, tt.fixture)

			got, err := s.UnlinkedEventsInRange(tt.start, tt.end)
			require.NoError(t, err)
			if d := cmp.Diff(tt.want, storetest.EventNames(got)); d != "" {
				t.Errorf("UnlinkedEventsInRange(%s, %s) (-want +got):\n%s", tt.start, tt.end, d)
			}
		})
	}
}

func TestEventsOn_ComparesOffsetsByInstant(t *testing.T) {
	// 20:00 PDT on the 6th is 03:00Z on the 7th.
	fixture := storetest.Fixture{Events: []events.Event{
		storetest.Event("jira").At(storetest.Pacific(20, 0, 0)).Build(),
	}}

	tests := []struct {
		name string
		day  time.Time
		want []string
	}{
		{name: "not on the day of its local wall clock", day: storetest.Midnight(6), want: []string{}},
		{name: "on its UTC day", day: storetest.Midnight(7), want: []string{"jira"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			storetest.Seed(t, s, fixture)

			got, err := s.EventsOn(tt.day)
			require.NoError(t, err)
			if d := cmp.Diff(tt.want, storetest.EventNames(got)); d != "" {
				t.Errorf("EventsOn(%s) (-want +got):\n%s", tt.day, d)
			}
		})
	}
}

// overlapFixture is three narratives around a [12:00Z, 15:00Z) window. "in" is given in
// PDT, so its text sorts before the window though its instants are inside it; "before"
// ends before the window starts, and "after" starts after it ends.
func overlapFixture() storetest.Fixture {
	return storetest.Fixture{Narratives: []storetest.NarrativeSpec{
		storetest.Narrative("in").Window(storetest.Pacific(6, 0, 0), storetest.Pacific(7, 0, 0)).Build(),
		storetest.Narrative("before").Window(storetest.UTC(10, 0, 0), storetest.UTC(11, 0, 0)).Build(),
		storetest.Narrative("after").Window(storetest.UTC(16, 0, 0), storetest.UTC(17, 0, 0)).Build(),
	}}
}

func TestNarrativesOverlapping_ComparesOffsetsByInstant(t *testing.T) {
	tests := []struct {
		name       string
		start, end time.Time
		want       []string
	}{
		{name: "offset windows, UTC bounds", start: storetest.UTC(12, 0, 0), end: storetest.UTC(15, 0, 0), want: []string{"in"}},
		{name: "offset bounds", start: storetest.Pacific(5, 0, 0), end: storetest.Pacific(8, 0, 0), want: []string{"in"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			storetest.Seed(t, s, overlapFixture())

			got, err := s.NarrativesOverlapping(tt.start, tt.end)
			require.NoError(t, err)
			if d := cmp.Diff(tt.want, storetest.NarrativeNames(got)); d != "" {
				t.Errorf("NarrativesOverlapping(%s, %s) (-want +got):\n%s", tt.start, tt.end, d)
			}
		})
	}
}

func TestNarrativesOverlappingExtended_ComparesOffsetsByInstant(t *testing.T) {
	tests := []struct {
		name       string
		fixture    storetest.Fixture
		start, end time.Time
		planned    map[string]time.Time
		want       []string
		wantEnd    map[string]time.Time
	}{
		{
			name: "an offset planned end brings the narrative into the window",
			fixture: storetest.Fixture{Narratives: []storetest.NarrativeSpec{
				storetest.Narrative("extended").Window(storetest.UTC(8, 0, 0), storetest.UTC(9, 0, 0)).Build(),
			}},
			start:   storetest.UTC(12, 0, 0),
			end:     storetest.UTC(14, 0, 0),
			planned: map[string]time.Time{"extended": storetest.Pacific(6, 0, 0)}, // 13:00Z
			want:    []string{"extended"},
			wantEnd: map[string]time.Time{"extended": storetest.UTC(13, 0, 0)},
		},
		{
			name: "an earlier offset planned end does not move the end back",
			fixture: storetest.Fixture{Narratives: []storetest.NarrativeSpec{
				storetest.Narrative("kept").Window(storetest.UTC(8, 0, 0), storetest.UTC(13, 0, 0)).Build(),
			}},
			start:   storetest.UTC(12, 0, 0),
			end:     storetest.UTC(14, 0, 0),
			planned: map[string]time.Time{"kept": storetest.Pacific(1, 30, 0)}, // 08:30Z
			want:    []string{"kept"},
			wantEnd: map[string]time.Time{"kept": storetest.UTC(13, 0, 0)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			ids := storetest.Seed(t, s, tt.fixture)
			planned := make(map[int64]time.Time, len(tt.planned))
			for name, end := range tt.planned {
				planned[ids[name]] = end
			}

			got, err := s.NarrativesOverlappingExtended(tt.start, tt.end, planned)
			require.NoError(t, err)
			if d := cmp.Diff(tt.want, storetest.NarrativeNames(got)); d != "" {
				t.Errorf("NarrativesOverlappingExtended names (-want +got):\n%s", d)
			}
			gotEnd := make(map[string]time.Time, len(got))
			for _, row := range got {
				gotEnd[row.Title] = row.WindowEnd
			}
			if d := cmp.Diff(tt.wantEnd, gotEnd); d != "" {
				t.Errorf("NarrativesOverlappingExtended window ends (-want +got):\n%s", d)
			}
		})
	}
}

func TestMemberEventsAfterBoundary_ComparesOffsetsByInstant(t *testing.T) {
	// A quarter-second apart: at whole seconds the two would tie, and only the id would
	// separate them.
	compacted := storetest.Event("compacted").At(storetest.UTC(20, 0, 0, 250000000))
	after := storetest.Event("after").At(storetest.UTC(20, 0, 0, 500000000))
	window := []time.Time{storetest.UTC(20, 0, 0), storetest.UTC(21, 0, 0)}

	tests := []struct {
		name    string
		fixture storetest.Fixture
		want    []string
	}{
		{
			name: "a boundary at the compacted event, given in PDT, keeps the event after it",
			fixture: storetest.Fixture{
				Events: []events.Event{compacted.Build(), after.Build()},
				Narratives: []storetest.NarrativeSpec{storetest.Narrative("n").Window(window[0], window[1]).
					Members("compacted", "after").CompactedAt("compacted", storetest.Pacific(13, 0, 0, 250000000)).Build()},
			},
			want: []string{"after"},
		},
		{
			name: "a boundary at the newest event keeps nothing",
			fixture: storetest.Fixture{
				Events: []events.Event{compacted.Build(), after.Build()},
				Narratives: []storetest.NarrativeSpec{storetest.Narrative("n").Window(window[0], window[1]).
					Members("compacted", "after").CompactedAt("after", storetest.Pacific(13, 0, 0, 500000000)).Build()},
			},
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			ids := storetest.Seed(t, s, tt.fixture)

			got, err := s.MemberEventsAfterBoundary(ids["n"])
			require.NoError(t, err)
			if d := cmp.Diff(tt.want, storetest.EventNames(got)); d != "" {
				t.Errorf("MemberEventsAfterBoundary (-want +got):\n%s", d)
			}
		})
	}
}

// -- latest readers ----------------------------------------------------------------

// newer is 20:00:00.000000001Z, given in PDT so its text sorts before older's.
var (
	newer = storetest.Pacific(13, 0, 0, 1)
	older = storetest.UTC(19, 0, 0)
)

// latestFixture holds one older UTC value and one newer PDT value for every reader
// that answers "the newest": two status changes on one issue, and two cursors.
func latestFixture() storetest.Fixture {
	return storetest.Fixture{
		Events: []events.Event{
			storetest.Event("newer").Source("jira").Issue("PROJ-1").StatusTo("Done").At(newer).Build(),
			storetest.Event("older").Source("jira").Issue("PROJ-1").StatusTo("In Progress").At(older).Build(),
		},
		Cursors: []storetest.CursorSpec{
			storetest.Cursor("/newer.jsonl").UpdatedAt(newer).Build(),
			storetest.Cursor("/older.jsonl").UpdatedAt(older).Build(),
		},
	}
}

func TestLatestStatusEvent_TakesTheNewestInstantAcrossOffsets(t *testing.T) {
	s := openStore(t)
	storetest.Seed(t, s, latestFixture())

	got, ok, err := s.LatestStatusEvent("PROJ-1")
	require.NoError(t, err)
	require.True(t, ok)
	if d := cmp.Diff(store.StatusEvent{To: "Done", OccurredAt: newer}, got); d != "" {
		t.Errorf("LatestStatusEvent (-want +got):\n%s", d)
	}
}

func TestIssueActivity_TakesTheNewestInstantAcrossOffsets(t *testing.T) {
	s := openStore(t)
	storetest.Seed(t, s, latestFixture())

	got, err := s.IssueActivity()
	require.NoError(t, err)
	if d := cmp.Diff(map[string]time.Time{"PROJ-1": newer}, got); d != "" {
		t.Errorf("IssueActivity (-want +got):\n%s", d)
	}
}

func TestEventCountsBySource_TakesTheNewestInstantAcrossOffsets(t *testing.T) {
	s := openStore(t)
	storetest.Seed(t, s, latestFixture())

	got, err := s.EventCountsBySource()
	require.NoError(t, err)
	if d := cmp.Diff([]store.SourceCount{{Source: "jira", Count: 2, Latest: newer}}, got); d != "" {
		t.Errorf("EventCountsBySource (-want +got):\n%s", d)
	}
}

func TestCursorCounts_TakesTheNewestInstantAcrossOffsets(t *testing.T) {
	s := openStore(t)
	storetest.Seed(t, s, latestFixture())

	got, err := s.CursorCounts()
	require.NoError(t, err)
	if d := cmp.Diff([]store.CollectorCount{{Collector: "claude_code", Count: 2, Latest: newer}}, got); d != "" {
		t.Errorf("CursorCounts (-want +got):\n%s", d)
	}
}

// -- the stored format's properties ------------------------------------------------

// TestStoredTimes_TextOrderIsInstantOrder pins the property every time comparison in
// this package leans on, so a driver upgrade cannot change it silently. The driver
// trims trailing fractional zeros, so stored values differ in width, and ordering them
// as text is right only because every value ends in "+00:00" and '+' sorts below '.'
// and every digit. Mixed offsets, mixed precisions and exact ties, at a size where a
// counterexample would show.
func TestStoredTimes_TextOrderIsInstantOrder(t *testing.T) {
	s := openStore(t)
	rng := rand.New(rand.NewPCG(53, 53))
	base := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	precisions := []time.Duration{time.Second, time.Millisecond, time.Microsecond, 10 * time.Nanosecond, time.Nanosecond}

	want := make(map[string]time.Time)
	for i := range 5000 {
		// Few enough distinct seconds that many values share one, and differ only in
		// the fraction.
		at := base.Add(time.Duration(rng.Int64N(int64(20 * time.Minute)))).
			Truncate(precisions[rng.IntN(len(precisions))])
		if i%50 == 1 {
			at = want[fmt.Sprintf("e%d", i-1)] // an exact tie, from a different zone below
		}
		zone := time.FixedZone("", (rng.IntN(105)-48)*15*60) // UTC-12:00 to UTC+14:00
		id := fmt.Sprintf("e%d", i)
		want[id] = at
		seedEvent(t, s, id, "e", at.In(zone))
	}

	// A clock reading, as Store.now gives one: the local zone and a monotonic reading,
	// which the stored value cannot carry and must drop without changing the instant.
	now := time.Now()
	want["now"] = now
	seedEvent(t, s, "now", "e", now)
	start, end := base, base.Add(time.Hour)
	if now.Before(start) {
		start = now
	}
	if !now.Before(end) {
		end = now.Add(time.Nanosecond)
	}

	got, err := s.UnlinkedEventsInRange(start, end)
	require.NoError(t, err)
	require.Len(t, got, len(want))
	for i, e := range got {
		require.True(t, want[e.ExternalID].Equal(e.OccurredAt),
			"%s round-trips exactly: got %s, want %s", e.ExternalID, e.OccurredAt, want[e.ExternalID])
		if i > 0 {
			require.False(t, e.OccurredAt.Before(got[i-1].OccurredAt),
				"ORDER BY occurred_at is instant order: %s sorted after %s", e.OccurredAt, got[i-1].OccurredAt)
		}
	}

	texts, err := s.QueryStringsForTest(`SELECT CAST(occurred_at AS TEXT) FROM events`)
	require.NoError(t, err)
	for _, text := range texts {
		require.True(t, strings.HasSuffix(text, "+00:00"), "%q is stored in UTC", text)
	}
	tied, err := s.QueryStringsForTest(
		`SELECT CAST(occurred_at AS TEXT) FROM events WHERE external_id IN ('e0', 'e1')`)
	require.NoError(t, err)
	require.Len(t, tied, 2)
	assert.Equal(t, tied[0], tied[1], "one instant has one text, whatever zone it was given in")
}

// storeTimeColumns is every column this package stores a time in. Each must be declared
// DATETIME, which is what makes the driver bind and scan it as a time.Time.
var storeTimeColumns = map[string][]string{
	"events":                 {"occurred_at", "ingested_at"},
	"cursors":                {"updated_at"},
	"narratives":             {"window_start", "window_end", "compaction_boundary", "created_at"},
	"narrative_events":       {"linked_at"},
	"narrative_issues":       {"created_at"},
	"actions":                {"decided_at", "executed_at", "created_at"},
	"correction_marks":       {"marked_at"},
	"pipeline_lock":          {"held_since", "expires_at"},
	"match_examinations":     {"examined_at"},
	"reconcile_examinations": {"examined_at"},
	"create_examinations":    {"examined_at"},
	"local_issues":           {"created_at", "updated_at"},
	"local_issue_comments":   {"created_at"},
}

// TestStore_EveryTimestampIsOneShapeAfterAFullWriteCycle runs every write path that
// stamps a time, then reads every column of every table as text. Any value shaped like
// a date must be a DATETIME column's, in the driver's UTC layout: a strftime default,
// an RFC 3339 string or a value in its source's offset would each show here.
func TestStore_EveryTimestampIsOneShapeAfterAFullWriteCycle(t *testing.T) {
	s := openStore(t)
	at := storetest.Pacific(13, 37, 35, 123000000)

	seedEvent(t, s, "e1", "e1", at)
	seedEvent(t, s, "e2", "e2", at.Add(time.Minute))
	e1, err := s.EventIDByExternalID("claude_code", "e1")
	require.NoError(t, err)
	e2, err := s.EventIDByExternalID("claude_code", "e2")
	require.NoError(t, err)
	require.NoError(t, s.SetCursor("claude_code", "/a.jsonl", "1:2"))

	n1, err := s.InsertNarrative(at, at.Add(time.Hour), "one", "s")
	require.NoError(t, err)
	n2, err := s.InsertNarrative(at, at.Add(time.Hour), "two", "s")
	require.NoError(t, err)
	require.NoError(t, s.ExtendNarrative(n1, at.Add(2*time.Hour), "s2"))
	require.NoError(t, s.LinkMembers(n1, []int64{e1}, 0.9))
	require.NoError(t, s.LinkContext(n2, []int64{e1}))
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.MoveMember(n2, e2, 0.8, store.PlacedByModel)
	}))
	require.NoError(t, s.SetCompactionBoundary(n1, at, e1, "recap"))
	require.NoError(t, s.AddNarrativeIssues(n1, []store.NarrativeIssue{
		{IssueKey: "PROJ-1", Role: store.RolePrimary, Provenance: "branch", Confidence: 0.9},
	}))

	action := func() int64 {
		id, err := s.InsertAction(store.ActionRow{
			NarrativeID: n1, Type: "comment", IssueKey: "PROJ-1", Payload: `{"body":"b"}`, Status: store.StatusProposed,
		})
		require.NoError(t, err)

		return id
	}
	applied := action()
	require.NoError(t, s.UpdateActionStatus(applied, store.StatusApproved))
	require.NoError(t, s.UpdateActionStatus(applied, store.StatusApplied))
	require.NoError(t, s.RecordRuling(action(), store.StatusRejected, "a lesson"))
	_, err = s.SupersedeAction(action(), store.StatusEdited, "reworded", store.ActionRow{
		NarrativeID: n1, Type: "comment", IssueKey: "PROJ-1", Payload: `{"body":"c"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)

	require.NoError(t, s.RecordMatchExamined(n2, "no candidate keys"))
	require.NoError(t, s.RecordReconcileExamined(n1, "self-authored"))
	require.NoError(t, s.RecordCreateExamined(n2, "self-authored"))
	ok, err := s.TryAcquire("run", at, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	key, err := s.InsertLocalIssue("LOC", "summary", "Task", "", nil)
	require.NoError(t, err)
	require.NoError(t, s.SetLocalIssueStatus(key, "Done"))
	require.NoError(t, s.InsertLocalIssueComment(key, "c"))

	tables, err := s.QueryStringsForTest(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	require.NoError(t, err)
	for _, table := range tables {
		declared := make(map[string]string)
		cols, err := s.QueryStringsForTest(`SELECT name || '|' || type FROM pragma_table_info(?)`, table)
		require.NoError(t, err)
		for _, c := range cols {
			name, typ, _ := strings.Cut(c, "|")
			declared[name] = typ
		}
		for _, col := range storeTimeColumns[table] {
			assert.Equal(t, "DATETIME", declared[col], "%s.%s is a time column", table, col)
		}

		for col, typ := range declared {
			values, err := s.QueryStringsForTest(fmt.Sprintf(
				`SELECT CAST(%[1]q AS TEXT) FROM %[2]q
				  WHERE CAST(%[1]q AS TEXT) GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*'`, col, table))
			require.NoError(t, err)
			for _, v := range values {
				assert.Equal(t, "DATETIME", typ, "%s.%s holds the date-shaped %q", table, col, v)
				assert.True(t, utcDatetime.MatchString(v), "%s.%s holds %q, not the one stored layout", table, col, v)
			}
			if typ == "DATETIME" {
				n, err := s.QueryStringForTest(fmt.Sprintf(`SELECT COUNT(%q) FROM %q`, col, table))
				require.NoError(t, err)
				assert.Equal(t, n, strconv.Itoa(len(values)), "every %s.%s value is date-shaped", table, col)
			}
		}
	}
}

// utcDatetime is the driver's _time_format=sqlite layout in UTC:
// "2006-01-02 15:04:05.999999999+00:00", the fraction absent when it is zero.
var utcDatetime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(\.\d*[1-9])?\+00:00$`)

// -- old stores --------------------------------------------------------------------

// TestOpen_StoreFormat: the store format marker decides whether an existing database
// opens. A store written before UTC timestamps has every column this build reads, so
// only its marker tells it apart; it must be refused before any schema statement, and
// the refusal must name the fix. A file with no tables holds no timestamps, so it is a
// fresh store, not an old one.
func TestOpen_StoreFormat(t *testing.T) {
	tests := []struct {
		name string
		file storetest.StoreFile
		// wantErr is every fragment the refusal contains beyond the path; nil means
		// Open succeeds.
		wantErr []string
		// absent are tables that must still not exist after Open: a refused open runs
		// no schema statement.
		absent []string
		// reopens is whether a second Open of the same file must succeed too.
		reopens bool
	}{
		{
			name:    "an old-format store is refused, before any schema statement",
			file:    storetest.StoreFile{Format: 0, Tables: true, Without: []string{"create_examinations"}},
			wantErr: []string{"UTC timestamps (finding F53)", "rename the database to a backup", "re-collect"},
			absent:  []string{"create_examinations"},
		},
		{
			name:    "an empty database file is a fresh store",
			file:    storetest.StoreFile{Format: 0, Tables: false},
			reopens: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unjira.db")
			storetest.WriteStoreFile(t, path, tt.file)

			s, err := store.Open(path)
			if s != nil {
				require.NoError(t, s.Close())
			}
			if tt.wantErr != nil {
				require.ErrorContains(t, err, path, "names which database")
				for _, fragment := range tt.wantErr {
					require.ErrorContains(t, err, fragment)
				}
			} else {
				require.NoError(t, err)
			}

			tables := storetest.Tables(t, path)
			for _, table := range tt.absent {
				assert.NotContains(t, tables, table, "a refused open must not create any table")
			}

			if tt.reopens {
				s, err := store.Open(path)
				require.NoError(t, err, "a store this build created reopens")
				require.NoError(t, s.Close())
			}
		})
	}
}
