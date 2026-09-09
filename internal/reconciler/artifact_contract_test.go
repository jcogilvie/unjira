package reconciler_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/reconciler"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// contractFakeTracker is a minimal TaskReader, distinct from
// reconciler_test's in-package fakeTracker, which this external test package
// cannot see.
type contractFakeTracker struct {
	issue tasktracker.Issue
}

func (f *contractFakeTracker) GetIssue(string) (tasktracker.Issue, error) { return f.issue, nil }

func (f *contractFakeTracker) AvailableTransitions(string) ([]tasktracker.Transition, error) {
	return nil, nil
}

func (f *contractFakeTracker) SearchIssues(string, int) ([]tasktracker.Issue, error) {
	return nil, nil
}

var _ tasktracker.TaskReader = (*contractFakeTracker)(nil)

// noCallLLM fails the test if drafting ever calls it — proof that the delta
// really was empty rather than merely reported so.
type noCallLLM struct{}

func (noCallLLM) Complete(context.Context, string, string) (string, llm.Usage, error) {
	panic("no LLM call should happen when the delta is empty")
}

var _ llm.Client = noCallLLM{}

// TestReconcile_DropsSelfAuthoredFromTheRealJiraCollectorsEvent drives the
// REAL collector/jira emitter into the public Reconcile entry point, rather
// than a hand-built events.Event with the artifact set inline (as
// reconciler_test's own
// TestReconcileDropsSelfAuthoredEventsBeforeComputingTheDelta does via its
// unjiraComment helper).
//
// task #182/F2: dropSelfAuthored's only signal is
// events.ArtifactAuthoredByUnjira, written by collector/jira and read by
// reconciler. A unit test on either side alone proves that side's own
// spelling is self-consistent; neither can catch the two packages
// disagreeing about the literal, which is exactly the risk a bare
// "authored_by_unjira" string on each side carried. This test fails if
// either side reverts to a literal that no longer matches the other.
func TestReconcile_DropsSelfAuthoredFromTheRealJiraCollectorsEvent(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "did the work", "summary")
	require.NoError(t, err)

	ic := collectorjira.IssueContext{Key: "PROJ-1", SelfAccountID: "acct-unjira"}
	selfEvt, err := collectorjira.EventFromComment(ic, map[string]any{
		"id":      "9001",
		"created": "2026-08-25T09:15:00.000+0000",
		"body":    "Narrated by unjira.",
		"author":  map[string]any{"accountId": "acct-unjira", "displayName": "unjira"},
	})
	require.NoError(t, err)

	_, err = s.InsertEvent(selfEvt)
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID(selfEvt.Source, selfEvt.ExternalID)
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(nid, []store.NarrativeIssue{{
			IssueKey: "PROJ-1", Role: "primary", Provenance: "branch", Confidence: 0.9,
		}})
	}))

	tracker := &contractFakeTracker{issue: tasktracker.Issue{Key: "PROJ-1", Summary: "the ticket"}}

	results, _, err := reconciler.Reconcile(
		t.Context(), s, tracker, noCallLLM{},
		config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5},
	)
	require.NoError(t, err)

	require.Len(t, results, 1)
	assert.True(t, results[0].SkippedNoDelta,
		"a delta consisting only of the real collector's self-authored event must read as empty")
}
