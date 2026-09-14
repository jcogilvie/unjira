package jira_test

// truncation_test.go covers what a per-query issue cap does to the watermark.
//
// The bug this pins was found in production data, not by reading code. A real
// pass hit `max_issues_per_query: 50` against ~114 matching issues, collected an
// ARBITRARY 50 of them (the JQL carries no ORDER BY, so which 50 is up to Jira),
// and then advanced the watermark to the newest issue it happened to see —
// 2026-09-11. Every unexamined issue older than that was thereafter excluded by
// `updated >= <watermark>` on every future pass. Resetting the cursor by hand
// recovered 56 issues that had been silently orphaned: 43 issues before, 99 after.
//
// The collector's own comment claimed the opposite:
//
//	"Because the watermark only advances on a complete query, the next pass
//	 re-runs this range rather than skipping ahead."
//
// That was never true of the code — SetCursor runs unconditionally — and nothing
// tested it. The pre-existing cap test asserts only that a log line appears,
// which is why a false claim about cursor behaviour survived next to code that
// contradicted it.
//
// This is the silent-data-loss class CLAUDE.md names as the hardest to notice:
// the pass logs a warning, reports success, and permanently skips work.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
	"github.com/jcogilvie/unjira/internal/config"
)

// TestCollect_TruncatedPassDoesNotAdvanceTheWatermark is the fix.
//
// A pass that could not see everything must not record that it did. Leaving the
// watermark alone costs a re-fetch of already-stored events, which dedupe on
// (source, external_id) and are therefore free; advancing it costs work that is
// never collected at all, and nothing surfaces the loss.
//
// The asymmetry is the whole argument: an over-wide window is recoverable, a
// too-narrow one is not.
func TestCollect_TruncatedPassDoesNotAdvanceTheWatermark(t *testing.T) {
	fake := &fakeJira{
		accountID: "acct-unjira",
		issues: []map[string]any{
			searchIssue("PROJ-1", "PROJ", "2026-08-20T16:00:00.000+0000"),
			searchIssue("PROJ-2", "PROJ", "2026-08-21T16:00:00.000+0000"),
		},
	}

	// Cap of 2 with 2 issues available: len(issues) >= limit, so this pass is
	// indistinguishable from one that truncated a larger result set. That is the
	// real condition — the collector cannot tell "exactly full" from "more
	// waiting" without fetching limit+1, which it does not do here.
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"}, MaxIssuesPerQuery: 2,
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	_, err := collectAll(t, cc)
	require.NoError(t, err)

	position, err := cc.Store.GetCursor("jira", collectorjira.CursorResource("corp", "mine"))
	require.NoError(t, err)

	assert.Empty(t, position,
		"a pass that hit its issue cap must NOT store a watermark: it collected an "+
			"arbitrary subset (the JQL has no ORDER BY), so recording the newest issue it "+
			"happened to see would permanently exclude every older issue it missed")
}

// TestCollect_CompletePassAdvancesTheWatermark is the other half, and it is what
// stops the fix from disabling incremental collection entirely.
//
// A pass that saw everything matching its query has genuinely made progress, and
// must record it — otherwise every pass re-scans the whole window forever, which
// is the cost the watermark exists to avoid.
func TestCollect_CompletePassAdvancesTheWatermark(t *testing.T) {
	fake := &fakeJira{
		accountID: "acct-unjira",
		issues: []map[string]any{
			searchIssue("PROJ-1", "PROJ", "2026-08-20T16:00:00.000+0000"),
			searchIssue("PROJ-2", "PROJ", "2026-08-21T16:00:00.000+0000"),
		},
	}

	// Cap comfortably above the result count, so len(issues) < limit.
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"}, MaxIssuesPerQuery: 50,
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	_, err := collectAll(t, cc)
	require.NoError(t, err)

	position, err := cc.Store.GetCursor("jira", collectorjira.CursorResource("corp", "mine"))
	require.NoError(t, err)

	assert.NotEmpty(t, position,
		"a complete pass must store a watermark, or incremental collection never "+
			"advances and every pass rescans the whole window")
}
