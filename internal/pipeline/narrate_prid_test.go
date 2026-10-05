package pipeline_test

// narrate_prid_test.go covers the clustering pre-filter's pull-request identity join
// (F43): an unplaced event carrying the same events.ArtifactPullRequest as a member of
// exactly one open narrative joins it before clustering, and the model never sees it.
//
// The finding, measured: with the acceptance window cut at 2026-09-21, #72's and #73's
// :opened events were clustered in pass 1 and their :merged/:closed events, arriving in
// pass 2, were placed in a NEW narrative in all six two-pass reps. Both halves' summaries
// began with the identical "jcogilvie/unjira#72:", so an implicit identifier was not
// enough. Under watch a PR opens in one pass and merges in another: the steady state.
//
// PR events here come from the REAL github collector constructors, not hand-built maps,
// so a writer that stopped emitting the key the join reads fails these tests too.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghclient "github.com/jcogilvie/unjira/internal/clients/github"
	collectorgithub "github.com/jcogilvie/unjira/internal/collector/github"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

var (
	pridOpenedAt = time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	pridCut      = time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	pridMergedAt = time.Date(2026, 9, 22, 15, 0, 0, 0, time.UTC)
	pridPass1    = correlator.TimeRange{Start: pridOpenedAt.Add(-time.Hour), End: pridCut}
	pridPass2    = correlator.TimeRange{Start: pridCut, End: pridCut.Add(72 * time.Hour)}
)

// pridRef parses a repos[] entry, as the github collector's config does.
func pridRef(t *testing.T, entry string) ghclient.RepoRef {
	t.Helper()

	ref, err := ghclient.ParseRepoRef(entry)
	require.NoError(t, err)

	return ref
}

func pridPR(number int) ghclient.PullRequest {
	pr := ghclient.PullRequest{
		Number: number, Title: "Fix the cache eviction", State: "open",
		HTMLURL: "https://github.com/o/r/pull/" + strconv.Itoa(number), CreatedAt: pridOpenedAt,
	}
	pr.User.Login = "alice"
	pr.Head.Ref = "fix/cache"

	return pr
}

// pridCompletion is pr's one completion event of kind ("merged" or "closed") at `at`,
// as CompletionEvents builds it from a timeline entry with id.
func pridCompletion(t *testing.T, ref ghclient.RepoRef, pr ghclient.PullRequest, kind string, id int64, at time.Time) events.Event {
	t.Helper()

	pr.State = "closed"
	pr.ClosedAt = &at
	evts, err := collectorgithub.CompletionEvents(ref, pr, []ghclient.IssueEvent{{ID: id, Event: kind, CreatedAt: at}})
	require.NoError(t, err)
	require.Len(t, evts, 1)

	return evts[0]
}

func pridInsert(t *testing.T, s *store.Store, e events.Event) int64 {
	t.Helper()

	_, err := s.InsertEvent(e)
	require.NoError(t, err)
	id, err := s.EventIDByExternalID(e.Source, e.ExternalID)
	require.NoError(t, err)

	return id
}

// pridHolder seeds a narrative holding evts as members, as a pass-1 clustering would.
func pridHolder(t *testing.T, s *store.Store, title string, evts ...events.Event) int64 {
	t.Helper()

	nid, err := s.InsertNarrative(pridOpenedAt, pridOpenedAt, title, title+" summary")
	require.NoError(t, err)
	ids := make([]int64, 0, len(evts))
	for _, e := range evts {
		ids = append(ids, pridInsert(t, s, e))
	}
	require.NoError(t, s.LinkMembers(nid, ids, 0.9))

	return nid
}

func pridNotInAnyPrompt(t *testing.T, client *narrateLLM, e events.Event) {
	t.Helper()

	stamp := "occurred_at=" + e.OccurredAt.Format(time.RFC3339)
	for i, p := range client.prompts {
		assert.NotContains(t, p, stamp, "prompt %d must not carry %s: the model never sees it", i, e.ExternalID)
	}
}

// TestRunNarrate_TwoPassPRJoinsByIdentity is F43 end to end, through two real passes.
// Pass 1 clusters #7's :opened. Pass 2's :merged carries the same PR, so it joins that
// narrative before clustering: the model is never shown it, the narrative's window
// grows to cover it, and the reconciler sees it as new work, so it can propose the
// transition a merge implies.
func TestRunNarrate_TwoPassPRJoinsByIdentity(t *testing.T) {
	s := narrateStore(t)
	ref := pridRef(t, "o/r")
	opened := collectorgithub.OpenedEvent(ref, pridPR(7))
	merged := pridCompletion(t, ref, pridPR(7), "merged", 9001, pridMergedAt)
	pridInsert(t, s, opened)
	pridInsert(t, s, merged)
	seedNarrateEvent(t, s, "unrelated", "tidied the README", pridMergedAt.Add(time.Hour))

	client := &narrateLLM{responses: []string{
		`[{"kind":"new","title":"Cache eviction fix","summary":"opened #7","confidence":0.9,"event_indices":[0]}]`,
		// Pass 2 numbers 0 = unrelated (the one in-window candidate left), 1 = #7's
		// :opened (its narrative is context now, and :opened is an eligible member).
		`[{"kind":"new","title":"README","summary":"tidy","confidence":0.8,"event_indices":[0]}]`,
	}}

	first, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), pridPass1, pipeline.NarrateOptions{})
	require.NoError(t, err)
	require.Len(t, first.Narratives, 1)
	holder := first.Narratives[0].ID
	assert.Empty(t, first.PreAssigned, "pass 1: nothing held #7 yet, so the model placed :opened")

	second, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), pridPass2, pipeline.NarrateOptions{})
	require.NoError(t, err)

	require.Len(t, second.PreAssigned, 1)
	assert.Equal(t, merged.ExternalID, second.PreAssigned[0].Event.ExternalID)
	assert.Equal(t, holder, second.PreAssigned[0].NarrativeID)
	assert.Equal(t, "github.com/o/r#7", second.PreAssigned[0].PullRequest)
	assert.Equal(t, 1, second.UnlinkedEvents, "only the unrelated event was a clustering candidate")

	require.Len(t, client.prompts, 2)
	pridNotInAnyPrompt(t, client, merged)
	assert.Contains(t, client.prompts[1], "tidied the README")

	mergedID := mustNarrateEventID(t, s, merged)
	l, err := s.NarrativeEventLink(holder, mergedID)
	require.NoError(t, err)
	assert.Equal(t, store.LinkMember, l.Kind)
	require.NotNil(t, l.MemberConfidence)
	assert.InDelta(t, 1.0, *l.MemberConfidence, 1e-9, "member_confidence is 1.0: identity, not an estimate")
	assert.Equal(t, store.PlacedByIdentity, l.Placement, "and recorded as identity's, not the model's")

	row, err := s.GetNarrative(holder)
	require.NoError(t, err)
	assert.Equal(t, pridMergedAt, row.WindowEnd.UTC(), "window_end extends to cover the merge, as an extend would")
	assert.Equal(t, "opened #7", row.Summary,
		"accepted cost: the summary is not rewritten, because the model never saw the merge")

	delta, err := s.DeltaEvents(holder)
	require.NoError(t, err)
	assert.Contains(t, eventKeys(delta), correlator.EventKey(merged), "the merge is new work in the reconciler's delta")

	rendered := pipeline.RenderNarrateResult(second)
	assert.Contains(t, rendered, "identity 1 event(s) joined their pull request's narrative by exact identity")
	assert.Contains(t, rendered, "github/"+merged.ExternalID+" -> narrative "+strconv.FormatInt(holder, 10)+" (github.com/o/r#7)")
}

// TestRunNarrate_AnAllPreAssignedPassCostsNoModelCall: when every unplaced event joins
// by identity, nothing is left to cluster, and the pass must not call the model.
func TestRunNarrate_AnAllPreAssignedPassCostsNoModelCall(t *testing.T) {
	s := narrateStore(t)
	ref := pridRef(t, "o/r")
	holder := pridHolder(t, s, "PR 7", collectorgithub.OpenedEvent(ref, pridPR(7)))
	merged := pridCompletion(t, ref, pridPR(7), "merged", 9001, pridMergedAt)
	pridInsert(t, s, merged)
	client := &narrateLLM{responses: []string{`[]`}}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), pridPass2, pipeline.NarrateOptions{})

	require.NoError(t, err)
	assert.Empty(t, client.prompts)
	require.Len(t, got.PreAssigned, 1)
	assert.Equal(t, holder, got.PreAssigned[0].NarrativeID)
	assert.Zero(t, got.UnlinkedEvents)
}

// TestRunNarrate_PRIdentityFallsBackToTheModel: whenever the identity does not name
// exactly one open home, the event goes to the model exactly as it did before the join,
// and the fallback is reported with its reason. Never a guess.
func TestRunNarrate_PRIdentityFallsBackToTheModel(t *testing.T) {
	ref := pridRef(t, "o/r")

	tests := []struct {
		name string
		// seed builds the store and returns the event under test and the narratives the
		// fallback must report as holders.
		seed       func(t *testing.T, s *store.Store) (events.Event, []int64)
		wantReason pipeline.PRFallbackReason
	}{
		{
			// F43's existing damage: the PR is already split. Joining either half would
			// pick one at random; the model has to see it.
			name: "two narratives already hold the PR's work",
			seed: func(t *testing.T, s *store.Store) (events.Event, []int64) {
				t.Helper()

				a := pridHolder(t, s, "half a", collectorgithub.OpenedEvent(ref, pridPR(7)))
				b := pridHolder(t, s, "half b", pridCompletion(t, ref, pridPR(7), "closed", 8001, pridOpenedAt.Add(time.Hour)))

				return pridCompletion(t, ref, pridPR(7), "merged", 9001, pridMergedAt), []int64{a, b}
			},
			wantReason: pipeline.PRSeveralHolders,
		},
		{
			name: "the one holder is split",
			seed: func(t *testing.T, s *store.Store) (events.Event, []int64) {
				t.Helper()

				n := pridHolder(t, s, "split", collectorgithub.OpenedEvent(ref, pridPR(7)))
				require.NoError(t, s.SetNarrativeStatus(n, store.StatusSplit))

				return pridCompletion(t, ref, pridPR(7), "merged", 9001, pridMergedAt), []int64{n}
			},
			wantReason: pipeline.PRHolderNotOpen,
		},
		{
			// Not a status unjira writes today. An allowlist, so a lifecycle state this
			// code has never seen is not treated as open.
			name: "the one holder has an unknown status",
			seed: func(t *testing.T, s *store.Store) (events.Event, []int64) {
				t.Helper()

				n := pridHolder(t, s, "closed", collectorgithub.OpenedEvent(ref, pridPR(7)))
				require.NoError(t, s.SetNarrativeStatus(n, "closed"))

				return pridCompletion(t, ref, pridPR(7), "merged", 9001, pridMergedAt), []int64{n}
			},
			wantReason: pipeline.PRHolderNotOpen,
		},
		{
			// acme/infra#12 on github.com and on GHES are two pull requests.
			name: "the holder's PR has the same owner/repo#N on another host",
			seed: func(t *testing.T, s *store.Store) (events.Event, []int64) {
				t.Helper()

				pridHolder(t, s, "github.com #7", collectorgithub.OpenedEvent(ref, pridPR(7)))

				return pridCompletion(t, pridRef(t, "ghes.corp.example/o/r"), pridPR(7), "merged", 9001, pridMergedAt), []int64{}
			},
			wantReason: pipeline.PRNoHolder,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := narrateStore(t)
			evt, holders := tt.seed(t, s)
			pridInsert(t, s, evt)
			client := &narrateLLM{responses: []string{
				`[{"kind":"new","title":"Merged","summary":"s","confidence":0.9,"event_indices":[0]}]`,
			}}

			got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), pridPass2, pipeline.NarrateOptions{})

			require.NoError(t, err)
			assert.Empty(t, got.PreAssigned, "nothing joins by identity")
			require.Len(t, client.prompts, 1)
			assert.Contains(t, client.prompts[0], "occurred_at="+pridMergedAt.Format(time.RFC3339),
				"the event goes to the model, unchanged")

			require.Len(t, got.PreAssignFallbacks, 1)
			fb := got.PreAssignFallbacks[0]
			assert.Equal(t, evt.ExternalID, fb.Event.ExternalID)
			assert.Equal(t, tt.wantReason, fb.Reason)
			reported := make([]int64, 0, len(fb.Holders))
			for _, h := range fb.Holders {
				reported = append(reported, h.NarrativeID)
			}
			assert.Equal(t, holders, reported, "every holder is reported, so the reason can be checked")

			if tt.wantReason != pipeline.PRNoHolder {
				assert.Contains(t, pipeline.RenderNarrateResult(got), string(tt.wantReason))
			}
		})
	}
}

// TestRunNarrate_AnUnconfirmedAnchorNeverJoins: an anchor whose result named no PR URL
// carries no events.ArtifactPullRequest (the collector's TestAnchor_FlagFallback pins
// that), so it has no identity to join on, however plainly its arguments name the repo.
// It reaches the model, and is not even a fallback: it never had an identity.
func TestRunNarrate_AnUnconfirmedAnchorNeverJoins(t *testing.T) {
	s := narrateStore(t)
	pridHolder(t, s, "PR 7", collectorgithub.OpenedEvent(pridRef(t, "o/r"), pridPR(7)))
	anchor := events.NewEvent("claude_code", "pr_create:toolu_01Unconfirmed", pridMergedAt,
		"session in r ran gh pr create (repo o/r, head fix/cache); its output named no pull request URL")
	anchor.Artifacts["anchor_kind"] = "pr_create"
	anchor.Artifacts["pr_create_outcome"] = "unconfirmed"
	anchor.Artifacts["pr_repo"] = "o/r"
	anchor.Artifacts["pr_head_branch"] = "fix/cache"
	pridInsert(t, s, anchor)
	client := &narrateLLM{responses: []string{
		`[{"kind":"new","title":"Attempt","summary":"s","confidence":0.6,"event_indices":[0]}]`,
	}}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), pridPass2, pipeline.NarrateOptions{})

	require.NoError(t, err)
	assert.Empty(t, got.PreAssigned)
	assert.Empty(t, got.PreAssignFallbacks)
	require.Len(t, client.prompts, 1)
	assert.Contains(t, client.prompts[0], "its output named no pull request URL")
}

// TestRunNarrate_AReopenedPRsSecondCloseJoins: a PR closed, reopened and closed again
// mints a second :closed with its own timeline id. It is the same PR, so it joins the
// narrative already holding the first close.
func TestRunNarrate_AReopenedPRsSecondCloseJoins(t *testing.T) {
	s := narrateStore(t)
	ref := pridRef(t, "o/r")
	holder := pridHolder(t, s, "PR 7",
		collectorgithub.OpenedEvent(ref, pridPR(7)),
		pridCompletion(t, ref, pridPR(7), "closed", 8001, pridOpenedAt.Add(time.Hour)))
	reclosed := pridCompletion(t, ref, pridPR(7), "closed", 8002, pridMergedAt)
	pridInsert(t, s, reclosed)
	client := &narrateLLM{responses: []string{`[]`}}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), pridPass2, pipeline.NarrateOptions{})

	require.NoError(t, err)
	require.Len(t, got.PreAssigned, 1)
	assert.Equal(t, reclosed.ExternalID, got.PreAssigned[0].Event.ExternalID)
	assert.Equal(t, holder, got.PreAssigned[0].NarrativeID)
	assert.Empty(t, client.prompts)
}

// TestRunNarrate_SeveralPlacementsExtendToTheLatest: two events joining one narrative
// move its window_end to the later of them, whichever the join met first. Candidates
// arrive oldest first, so keeping the first placement's time would leave the window
// short of the second.
func TestRunNarrate_SeveralPlacementsExtendToTheLatest(t *testing.T) {
	s := narrateStore(t)
	ref := pridRef(t, "o/r")
	holder := pridHolder(t, s, "PR 7", collectorgithub.OpenedEvent(ref, pridPR(7)))
	closedAt := pridMergedAt.Add(-time.Hour)
	pridInsert(t, s, pridCompletion(t, ref, pridPR(7), "closed", 8001, closedAt))
	pridInsert(t, s, pridCompletion(t, ref, pridPR(7), "closed", 8002, pridMergedAt))

	got, err := pipeline.RunNarrate(t.Context(), s, &narrateLLM{responses: []string{`[]`}},
		narrateConfig(), pridPass2, pipeline.NarrateOptions{})

	require.NoError(t, err)
	require.Len(t, got.PreAssigned, 2)
	row, err := s.GetNarrative(holder)
	require.NoError(t, err)
	assert.Equal(t, pridMergedAt, row.WindowEnd.UTC(), "window_end covers the later placement")
}

// TestRunNarrate_AFrozenTargetAcceptsTheMergeAsNewWork: the narrative holding #7 has
// already posted to the tracker, so its :opened is frozen. The merge is not frozen —
// no posted mutation describes it — so it joins as a NEW member link, past the posted
// action, and is in the delta: the reconciler can propose the transition a merge
// implies. A join that reused the narrative's committed position would hide it.
func TestRunNarrate_AFrozenTargetAcceptsTheMergeAsNewWork(t *testing.T) {
	s := narrateStore(t)
	ref := pridRef(t, "o/r")
	holder := pridHolder(t, s, "PR 7", collectorgithub.OpenedEvent(ref, pridPR(7)))
	commitAction(t, s, holder)
	merged := pridCompletion(t, ref, pridPR(7), "merged", 9001, pridMergedAt)
	pridInsert(t, s, merged)

	got, err := pipeline.RunNarrate(t.Context(), s, &narrateLLM{responses: []string{`[]`}},
		narrateConfig(), pridPass2, pipeline.NarrateOptions{})

	require.NoError(t, err)
	require.Len(t, got.PreAssigned, 1)
	delta, err := s.DeltaEvents(holder)
	require.NoError(t, err)
	assert.Equal(t, []string{correlator.EventKey(merged)}, eventKeys(delta),
		"the merge, and only the merge, is new work since the posted action")
	eligible, err := s.EligibleMemberEvents(holder)
	require.NoError(t, err)
	assert.Equal(t, []string{correlator.EventKey(merged)}, eventKeys(eligible),
		":opened stays frozen; the merge is reshufflable like any uncommitted member")
}

// TestRunNarrate_DryRunReportsThePlacementAndWritesNothing: a dry run shows what identity
// would place, and leaves the event unplaced.
func TestRunNarrate_DryRunReportsThePlacementAndWritesNothing(t *testing.T) {
	s := narrateStore(t)
	ref := pridRef(t, "o/r")
	holder := pridHolder(t, s, "PR 7", collectorgithub.OpenedEvent(ref, pridPR(7)))
	merged := pridCompletion(t, ref, pridPR(7), "merged", 9001, pridMergedAt)
	pridInsert(t, s, merged)

	got, err := pipeline.RunNarrate(t.Context(), s, &narrateLLM{responses: []string{`[]`}},
		narrateConfig(), pridPass2, pipeline.NarrateOptions{DryRun: true})

	require.NoError(t, err)
	require.Len(t, got.PreAssigned, 1)
	assert.Equal(t, holder, got.PreAssigned[0].NarrativeID)
	unlinked, err := s.UnlinkedEventsInRange(pridPass2.Start, pridPass2.End)
	require.NoError(t, err)
	assert.Equal(t, []string{correlator.EventKey(merged)}, eventKeys(unlinked), "nothing persisted")
	row, err := s.GetNarrative(holder)
	require.NoError(t, err)
	assert.Equal(t, pridOpenedAt, row.WindowEnd.UTC())
}

// TestRunNarrate_DryRunClustersAgainstTheRealPassesContext is F46. The holder of #7
// ends before pass 2's window, and the join places #7's :merged into it. The real pass
// writes first, which moves the holder's window_end into the window, so the holder is
// clustering context. The dry run writes nothing, yet must show the model the same
// prompt, or "exactly what would have been persisted" previews a different pass.
//
// The bounded case is why the planned extension must reach the store's query rather
// than be patched on after it: with room for one context narrative, the extended holder
// ends latest and outranks a competitor, but only if the bound ranks its extended
// window_end.
func TestRunNarrate_DryRunClustersAgainstTheRealPassesContext(t *testing.T) {
	ref := pridRef(t, "o/r")
	merged := pridCompletion(t, ref, pridPR(7), "merged", 9001, pridMergedAt)

	tests := []struct {
		name string
		// extra seeds anything beyond the holder, the merge and the unrelated candidate.
		extra      func(t *testing.T, s *store.Store)
		maxContext int
		wantAbsent string
	}{
		{name: "the holder ends before the window"},
		{
			name:       "the bound ranks the holder by its extended window_end",
			maxContext: 1,
			extra: func(t *testing.T, s *store.Store) {
				t.Helper()

				_, err := s.InsertNarrative(pridCut, pridCut.Add(time.Hour), "Competitor", "overlaps the window")
				require.NoError(t, err)
			},
			wantAbsent: "Competitor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seed := func(t *testing.T, s *store.Store) {
				t.Helper()

				pridHolder(t, s, "PR 7", collectorgithub.OpenedEvent(ref, pridPR(7)))
				pridInsert(t, s, merged)
				seedNarrateEvent(t, s, "unrelated", "tidied the README", pridMergedAt.Add(time.Hour))
				if tt.extra != nil {
					tt.extra(t, s)
				}
			}
			run := func(t *testing.T, s *store.Store, dryRun bool) (string, pipeline.NarrateResult) {
				t.Helper()

				cfg := narrateConfig()
				cfg.Correlator.MaxContextNarratives = tt.maxContext
				client := &narrateLLM{responses: []string{
					`[{"kind":"new","title":"README","summary":"tidy","confidence":0.8,"event_indices":[0]}]`,
				}}
				got, err := pipeline.RunNarrate(t.Context(), s, client, cfg, pridPass2, pipeline.NarrateOptions{DryRun: dryRun})
				require.NoError(t, err)
				require.Len(t, client.prompts, 1)

				return client.prompts[0], got
			}

			realStore := narrateStore(t)
			seed(t, realStore)
			realPrompt, persisted := run(t, realStore, false)

			dryPath := filepath.Join(t.TempDir(), "dry.db")
			dryStore, err := store.Open(dryPath)
			require.NoError(t, err)
			t.Cleanup(func() { _ = dryStore.Close() })
			seed(t, dryStore)
			before, err := os.ReadFile(dryPath)
			require.NoError(t, err)
			dryPrompt, dry := run(t, dryStore, true)
			after, err := os.ReadFile(dryPath)
			require.NoError(t, err)

			require.Len(t, dry.PreAssigned, 1, "the join fires in the dry run too")
			assert.Equal(t, realPrompt, dryPrompt, "the dry run shows the model exactly what the real pass does")
			holderLine := fmt.Sprintf("title=%q window=[%s, %s)", "PR 7",
				pridOpenedAt.Format(time.RFC3339), pridMergedAt.Format(time.RFC3339))
			assert.Contains(t, dryPrompt, holderLine, "the holder is context, with the window the join's write gives it")
			if tt.wantAbsent != "" {
				assert.NotContains(t, dryPrompt, tt.wantAbsent)
			}
			assert.Equal(t, persisted.ContextNarratives, dry.ContextNarratives)
			assert.Equal(t, persisted.ExcludedContextNarratives, dry.ExcludedContextNarratives)
			assert.True(t, bytes.Equal(before, after), "a dry run leaves the store file byte-for-byte untouched")
		})
	}
}

// TestRunNarrate_EventsFoldedCountsAPreAssignedMember: a placed event is hidden from
// the prompt but is in the store, so when the model extends the same narrative and
// Persist compacts it, the merge is part of the history being folded. EventsFolded must
// count it in the narrative's pre-pass visible total, or the report says one fewer
// event was folded than was.
func TestRunNarrate_EventsFoldedCountsAPreAssignedMember(t *testing.T) {
	s := narrateStore(t)
	ref := pridRef(t, "o/r")
	cfg := narrateConfig()
	cfg.Correlator.TailSummarizeThresholdTokens = 10
	cfg.Correlator.RecentEventsKept = 2
	holder := pridHolder(t, s, "PR 7",
		collectorgithub.OpenedEvent(ref, pridPR(7)),
		events.NewEvent("claude_code", "e1", pridOpenedAt.Add(time.Minute), "investigated the eviction"))
	merged := pridCompletion(t, ref, pridPR(7), "merged", 9001, pridMergedAt)
	pridInsert(t, s, merged)
	seedNarrateEvent(t, s, "e3", "followed up on the merge", pridMergedAt.Add(time.Hour))

	client := &narrateLLM{responses: []string{
		// 0 = e3 (in-window), 1 = :opened and 2 = e1 (the holder's eligible members).
		// The merge is not numbered: it was placed by identity.
		fmt.Sprintf(`[{"kind":"extends","narrative_id":%d,"summary":"s1","confidence":0.9,"event_indices":[0,1,2]}]`, holder),
		"recap of the opening",
	}}

	got, err := pipeline.RunNarrate(t.Context(), s, client, cfg, pridPass2, pipeline.NarrateOptions{})

	require.NoError(t, err)
	require.Len(t, got.PreAssigned, 1)
	assert.NotContains(t, client.prompts[0], "occurred_at="+pridMergedAt.Format(time.RFC3339),
		"the clustering prompt never carries the merge")
	require.Len(t, got.Compactions, 1)
	// History opened, e1, merged, e3; two kept, so :opened and e1 are folded.
	assert.Equal(t, 2, got.Compactions[0].EventsFolded)
}

func mustNarrateEventID(t *testing.T, s *store.Store, e events.Event) int64 {
	t.Helper()

	id, err := s.EventIDByExternalID(e.Source, e.ExternalID)
	require.NoError(t, err)

	return id
}

func eventKeys(evts []events.Event) []string {
	out := make([]string, 0, len(evts))
	for _, e := range evts {
		out = append(out, correlator.EventKey(e))
	}

	return out
}
