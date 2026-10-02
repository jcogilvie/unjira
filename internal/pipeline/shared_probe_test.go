package pipeline

// shared_probe_test.go is the shared-context acceptance harness
// (docs/superpowers/specs/2026-10-02-shared-context-design.md §9). Env-gated and
// skipped by default: it makes REAL model calls and WRITES the store it is pointed at,
// so point it at a fresh copy of the un-narrated snapshot, once per rep, never at
// data/unjira.db and never at another rep's output (#35):
//
//	cp snapshot.db rep1.db
//	SHARED_PROBE=1 SHARED_DB=$PWD/rep1.db \
//	  SHARED_START=2026-09-16T00:00:00Z SHARED_END=2026-10-02T00:00:00Z \
//	  go test ./internal/pipeline/ -run TestSharedContext_AcceptanceRep -v -count=1 -timeout 2h
//
// Add SHARED_CUT=<RFC3339 inside the window> for M7's two-pass scenario: pass 1
// narrates [start, cut), pass 2 [cut, end), both persisted. Run from the main
// checkout, where unjira.config.json and the credentials live (#37); the window is
// ABSOLUTE, unlike `dev narrate --since`, so reps do not drift.
//
// A failed pass prints DIED and the error, and the test fails: it never reports a
// number from a run that did not complete (#37). Every metric is read from the store
// afterwards (shared_probe_metrics_test.go), never from printed output (finding 3).
//
// THE BASELINE ARM uses this same file. Copy it and shared_probe_metrics_test.go into a
// checkout of main (which has f16_probe_test.go's realClient) and add the one stub
// main lacks, since the treatment-only reports (disputes, M6a) read fields and readers
// this slice added:
//
//	func reportTreatment(*testing.T, *store.Store, []NarrateResult, []probeLink) {}

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/store"
)

func probeTime(t *testing.T, name string) time.Time {
	t.Helper()

	v := os.Getenv(name)
	require.NotEmpty(t, v, "%s is required (RFC3339)", name)
	ts, err := time.Parse(time.RFC3339, v)
	require.NoError(t, err, "%s=%q", name, v)

	return ts
}

func TestSharedContext_AcceptanceRep(t *testing.T) {
	if os.Getenv("SHARED_PROBE") == "" {
		t.Skip("set SHARED_PROBE=1, SHARED_DB, SHARED_START and SHARED_END (and optionally SHARED_CUT) to run a rep")
	}

	dbPath := os.Getenv("SHARED_DB")
	require.NotEmpty(t, dbPath, "SHARED_DB must name a fresh COPY of the snapshot; this test writes it")
	window := correlator.TimeRange{Start: probeTime(t, "SHARED_START"), End: probeTime(t, "SHARED_END")}

	cfg, err := config.Load("../../unjira.config.json")
	require.NoError(t, err)
	client, err := realClient()
	require.NoError(t, err)

	s, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	passes := []correlator.TimeRange{window}
	if os.Getenv("SHARED_CUT") != "" {
		cut := probeTime(t, "SHARED_CUT")
		require.True(t, cut.After(window.Start) && cut.Before(window.End), "SHARED_CUT must fall inside the window")
		passes = []correlator.TimeRange{{Start: window.Start, End: cut}, {Start: cut, End: window.End}}
	}

	var pass1HighWater int64
	results := make([]NarrateResult, 0, len(passes))
	for i, w := range passes {
		res, err := RunNarrate(t.Context(), s, client, cfg, w, NarrateOptions{})
		if err != nil {
			fmt.Printf("\npass %d [%s, %s): DIED: %v\n", i+1, w.Start.Format(time.RFC3339), w.End.Format(time.RFC3339), err)
			t.FailNow()
		}
		printPassCost(i+1, res)
		results = append(results, res)
		if i == 0 {
			pass1HighWater = linkHighWater(t, dbPath)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	links, err := loadProbeLinks(db)
	require.NoError(t, err)
	workless, err := loadWorklessNarratives(db)
	require.NoError(t, err)

	fmt.Printf("\n%s", renderAcceptance(measureAcceptance(links, workless, window)))
	reportTreatment(t, s, results, links)

	if len(passes) == 2 {
		a := measureAttraction(links, pass1HighWater)
		fmt.Printf("M7  screen: %d pass-2 member placement(s) whose evidence points at another pass-1 narrative; "+
			"%d into a narrative holding that narrative's work as background (READ EACH: the screen is not the verdict)\n",
			len(a.Screened), len(a.Attracted))
		for _, l := range a.Screened {
			fmt.Printf("      %s\n", l)
		}
		for _, l := range a.Attracted {
			fmt.Printf("      ATTRACTED %s\n", l)
		}
	}
}

// printPassCost prints M5 for one pass. Only Stats fields main also has, so the baseline
// arm prints the same line; the dispute record is reportTreatment's.
func printPassCost(n int, r NarrateResult) {
	newCount := 0
	for _, nn := range r.Narratives {
		if nn.Kind == correlator.ClusterNew {
			newCount++
		}
	}
	perCluster := int64(0)
	if len(r.Narratives) > 0 {
		perCluster = r.Stats.CompletionTokens / int64(len(r.Narratives))
	}

	fmt.Printf("\npass %d: M5 %d prompt + %d completion tokens, %d/cluster, %d cluster(s), %d NEW, %d call(s), %d split(s)\n",
		n, r.Stats.PromptTokens, r.Stats.CompletionTokens, perCluster, len(r.Narratives), newCount,
		r.Stats.Calls, r.Stats.Splits)
}

// linkHighWater is the link sequence's high-water mark, read straight from SQLite.
func linkHighWater(t *testing.T, dbPath string) int64 {
	t.Helper()

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var hw sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'narrative_events'`).Scan(&hw))

	return hw.Int64
}
