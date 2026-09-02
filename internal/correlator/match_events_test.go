package correlator_test

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// This file is the regression suite for a real matching failure, reproduced from
// production data rather than invented. Narrative 19 of a 2026-09-01 pass over
// 300 real events held three events:
//
//	claude_code session on branch elasticache-replication-groups
//	  (ticket_keys: PAAS-3939, SUMO-287220, PAAS-4038, SUMO-287986)
//	jira: PAAS-4038 status Discovery -> Ready for Dev
//	jira: PAAS-4038 comment "PR open: .../pull/480"
//
// It ended with ZERO narrative_issues rows, so the create path then proposed a
// brand-new ticket for work already tracked in PAAS-4038 — a duplicate.
//
// Both SUMO keys are real, live issues on the same Jira site (verified via the
// API), so verification was not the problem: all four candidates resolved. The
// classifier returned an empty array and resolveVerified discarded everything,
// including PAAS-4038, whose provenance was jira_event — a recorded fact rather
// than an inference.
//
// The cause was in the prompt. buildMatchPrompt sent the narrative's title,
// summary, and each candidate's Jira metadata, but NOT the narrative's events —
// so the model could not see that two of the three events were Jira events about
// PAAS-4038 specifically. The prompt tells it to "judge from each candidate's
// summary, description, and status, not from provenance strength alone", which
// asks for evidence-based judgment while withholding the evidence.

// narrative19Tracker resolves every candidate, mirroring the live site where all
// four keys exist.
type narrative19Tracker struct{ fetched []string }

func (t *narrative19Tracker) GetIssue(key string) (tasktracker.Issue, error) {
	t.fetched = append(t.fetched, key)

	byKey := map[string]tasktracker.Issue{
		"PAAS-4038": {
			Key: key, StatusName: "Ready for Dev",
			Summary: "Add ElastiCache durability (synchronous writes) support to XElastiCache composition",
		},
		"PAAS-3939":   {Key: key, StatusName: "Done", Summary: "Earlier unrelated PAAS work"},
		"SUMO-287220": {Key: key, StatusName: "Done", Summary: "Post Release Action Items"},
		"SUMO-287986": {Key: key, StatusName: "Done", Summary: "Another release checklist"},
	}

	return byKey[key], nil
}

func (t *narrative19Tracker) AvailableTransitions(string) ([]tasktracker.Transition, error) {
	return nil, nil
}

func (t *narrative19Tracker) SearchIssues(string, int) ([]tasktracker.Issue, error) {
	return nil, nil
}

// promptCapturingLLM records what it was asked and answers with a canned verdict.
type promptCapturingLLM struct {
	response string
	user     string
	system   string
	calls    int
}

func (c *promptCapturingLLM) Complete(_ context.Context, system, user string) (string, llm.Usage, error) {
	c.calls++
	c.system, c.user = system, user

	return c.response, llm.Usage{}, nil
}

// seedNarrative19 recreates the narrative exactly as the real pass stored it.
func seedNarrative19(t *testing.T, s *store.Store) int64 {
	t.Helper()

	base := time.Date(2026, 8, 28, 15, 23, 30, 0, time.UTC)

	nid, err := s.InsertNarrative(base, base.Add(72*time.Hour),
		"PAAS-4038 / ElastiCache durability and replication group support",
		"PAAS-4038 PR implementing spec.durability on XElastiCache via provider-aws-elasticache, "+
			"ticket moved to Ready for Dev, plus related Claude Code work extending the "+
			"ElastiCache composition to support replication groups.")
	require.NoError(t, err)

	session := events.NewEvent("claude_code", "n19:session", base,
		"Claude Code session in elasticache-replication-groups on branch "+
			"elasticache-replication-groups: 8 user messages")
	session.Artifacts = map[string]any{
		"git_branch":  "elasticache-replication-groups",
		"ticket_keys": []any{"PAAS-3939", "SUMO-287220", "PAAS-4038", "SUMO-287986"},
	}

	transition := events.NewEvent("jira", "n19:transition", base.Add(66*time.Hour),
		"PAAS-4038 status: Discovery → Ready for Dev")
	transition.Artifacts = map[string]any{
		"issue_key": "PAAS-4038", "project_key": "PAAS", "connection": "dev",
		"field": "status", "authored_by_unjira": false,
	}

	comment := events.NewEvent("jira", "n19:comment", base.Add(67*time.Hour),
		"PAAS-4038 comment by Jon Ogilvie: PR open: https://github.com/Sanyaku/helm-charts/pull/480")
	comment.Artifacts = map[string]any{
		"issue_key": "PAAS-4038", "connection": "dev", "authored_by_unjira": false,
	}

	for _, e := range []events.Event{session, transition, comment} {
		_, err := s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID(e.Source, e.ExternalID)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
	}

	return nid
}

func narrative19Config() config.MatchConfig {
	return config.MatchConfig{MaxCandidatesPerNarrative: 10, ConfidenceFloor: 0.7}
}

// TestMatch_PromptCarriesTheNarrativesEvents is the fix. Without the events the
// model is asked which of four tickets owns this work while being shown only the
// keys and their Jira metadata — the two Jira events naming PAAS-4038 are exactly
// the evidence that settles it.
func TestMatch_PromptCarriesTheNarrativesEvents(t *testing.T) {
	s := persistStore(t)
	seedNarrative19(t, s)

	tracker := &narrative19Tracker{}
	client := &promptCapturingLLM{response: `[{"issue_key":"PAAS-4038","role":"primary",` +
		`"confidence":0.95,"rationale":"two events are status changes on this ticket"}]`}

	_, _, err := correlator.Match(context.Background(), s, tracker, client, narrative19Config())
	require.NoError(t, err)

	require.Equal(t, 1, client.calls, "one classifier call for a four-candidate narrative")

	assert.Contains(t, client.user, "PAAS-4038 status: Discovery → Ready for Dev",
		"the Jira transition is the decisive evidence and must reach the model")
	assert.Contains(t, client.user, "PR open: https://github.com/Sanyaku/helm-charts/pull/480",
		"the Jira comment names the PR, which is how the model can tell this work from a mention")
	assert.Contains(t, client.user, "elasticache-replication-groups",
		"the session event carries the branch name")
}

// TestMatch_LinksTheJiraEventCandidate is the outcome the real pass got wrong:
// PAAS-4038 becomes the primary and the narrative stops looking untracked.
func TestMatch_LinksTheJiraEventCandidate(t *testing.T) {
	s := persistStore(t)
	nid := seedNarrative19(t, s)

	tracker := &narrative19Tracker{}
	client := &promptCapturingLLM{response: `[` +
		`{"issue_key":"PAAS-4038","role":"primary","confidence":0.95,"rationale":"r"},` +
		`{"issue_key":"SUMO-287220","role":"mentioned","confidence":0.3,"rationale":"r"}]`}

	results, _, err := correlator.Match(context.Background(), s, tracker, client, narrative19Config())
	require.NoError(t, err)

	require.Len(t, results, 1)
	assert.Equal(t, "PAAS-4038", results[0].Primary,
		"the ticket two events are literally about must be the primary")

	links, err := s.NarrativeIssues(nid)
	require.NoError(t, err)
	require.Len(t, links, 2, "the primary plus the mention are both recorded")

	// And the narrative is no longer selected as untracked, which is what let the
	// create path propose a duplicate.
	untracked, err := s.NarrativesWithNoIssueLink(10)
	require.NoError(t, err)
	assert.Empty(t, untracked,
		"a linked narrative must not reach the create path as untracked work")
}

// TestMatch_EmptyClassifierResponseIsLoggedNotSilent. resolveVerified discards
// every candidate when the model returns [], which is how the real pass lost
// PAAS-4038 with no error and no log line. The prompt states "Exactly one
// candidate must receive this role", so an empty array violates a stated
// requirement — it is closer to a malformed response than to a verdict, and must
// at minimum be visible.
//
// Deliberately NOT a hard error yet: making it fail changes retry behaviour for a
// whole pass, and the prompt fix above may remove the case entirely. Visibility
// first, so the next real run tells us whether it still happens.
func TestMatch_EmptyClassifierResponseIsLoggedNotSilent(t *testing.T) {
	s := persistStore(t)
	nid := seedNarrative19(t, s)

	// log.SetOutput directly, matching internal/collector/jira's own tests rather
	// than adding an export_test seam for something the stdlib already exposes.
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	tracker := &narrative19Tracker{}
	client := &promptCapturingLLM{response: "[]"}

	results, _, err := correlator.Match(context.Background(), s, tracker, client, narrative19Config())

	require.NoError(t, err, "an empty response must not fail the pass")
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Primary)

	links, err := s.NarrativeIssues(nid)
	require.NoError(t, err)
	assert.Empty(t, links, "nothing is linked, which is the honest outcome")

	out := logged.String()
	assert.Contains(t, out, "classifier returned no verdicts",
		"the discard must be visible; silence is how this bug survived a real pass")
	assert.Contains(t, out, "4 verified candidate(s)",
		"the count is what shows the discard was total rather than a judgment about one key")
}

// TestMatch_SingleCandidateStillSkipsTheClassifier guards the existing shortcut:
// one verified candidate is deterministic and must not spend an LLM call. Included
// because this file changes the surrounding function and the shortcut is the
// reason narrative 19 was the unlucky case rather than the common one.
func TestMatch_SingleCandidateStillSkipsTheClassifier(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 28, 15, 0, 0, 0, time.UTC)

	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "one ticket only", "summary")
	require.NoError(t, err)

	e := events.NewEvent("jira", "single:1", base, "PAAS-4038 status: Discovery → Ready for Dev")
	e.Artifacts = map[string]any{"issue_key": "PAAS-4038", "connection": "dev"}
	_, err = s.InsertEvent(e)
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID("jira", "single:1")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))

	tracker := &narrative19Tracker{}
	client := &promptCapturingLLM{response: "[]"}

	results, _, err := correlator.Match(context.Background(), s, tracker, client, narrative19Config())

	require.NoError(t, err)
	assert.Zero(t, client.calls, "a lone candidate needs no model judgment")
	require.Len(t, results, 1)
	assert.Equal(t, "PAAS-4038", results[0].Primary)
}

// TestMatch_LoneJiraEventCandidateLinksDeterministically isolates the case a real
// pass got wrong AFTER the events-in-prompt fix. A clustering pass split the
// ElastiCache work so that one narrative held only the two Jira events about
// PAAS-4038 — a single candidate, which resolveVerified links at confidence 1.0
// without consulting the model at all. It still ended with zero links, and no
// "no verdicts" log fired, so the empty-response path was not involved.
//
// Reproduced here as a unit test rather than by re-running the pipeline: this
// asserts the deterministic shortcut against the exact two events, so a failure
// localizes the bug to our code and a pass localizes it to the environment.
func TestMatch_LoneJiraEventCandidateLinksDeterministically(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 28, 12, 42, 30, 0, time.UTC)

	nid, err := s.InsertNarrative(base, base.Add(67*time.Hour),
		"PAAS-4038 ElastiCache durability PR",
		"PAAS-4038 moved to Ready for Dev with a PR comment")
	require.NoError(t, err)

	// Exactly narrative 20's events, including the unjira-authored comment.
	transition := events.NewEvent("jira", "lone:transition", base.Add(66*time.Hour),
		"PAAS-4038 status: Discovery → Ready for Dev")
	transition.Artifacts = map[string]any{
		"authored_by_unjira": false, "connection": "dev", "field": "status",
		"issue_key": "PAAS-4038", "project_key": "PAAS",
	}

	comment := events.NewEvent("jira", "lone:comment", base.Add(67*time.Hour),
		"PAAS-4038 comment by Jon Ogilvie: PR open: https://github.com/Sanyaku/helm-charts/pull/480")
	comment.Artifacts = map[string]any{
		"authored_by_unjira": true, "connection": "dev",
		"issue_key": "PAAS-4038", "project_key": "PAAS",
	}

	for _, e := range []events.Event{transition, comment} {
		_, err := s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID(e.Source, e.ExternalID)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
	}

	tracker := &narrative19Tracker{}
	client := &promptCapturingLLM{response: "[]"}

	results, _, err := correlator.Match(context.Background(), s, tracker, client, narrative19Config())
	require.NoError(t, err)

	t.Logf("tracker fetched: %v", tracker.fetched)
	t.Logf("classifier calls: %d", client.calls)
	require.Len(t, results, 1)
	t.Logf("result: primary=%q links=%d unresolved=%v excluded=%v",
		results[0].Primary, len(results[0].Links), results[0].Unresolved, results[0].Excluded)

	assert.Zero(t, client.calls, "one candidate is deterministic; no model call")
	assert.Equal(t, "PAAS-4038", results[0].Primary,
		"a lone verified candidate must be linked at 1.0 without model judgment")

	links, err := s.NarrativeIssues(nid)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, "PAAS-4038", links[0].IssueKey)
}

// TestMatch_NarrativeCapIsSeparateFromCandidateCap is the second bug behind
// narrative 19's symptom. correlator.Match used one `limit` for both roles:
//
//	limit := cfg.CandidateLimit()                          // candidates per narrative
//	narratives, err := s.NarrativesWithoutIssueKey(limit)   // narratives per pass
//
// So a config with max_candidates_per_narrative=10 examined only 10 narratives
// per pass. On a real 30-narrative backlog that left 20 unmatched, which then
// reached the create path as untracked work and drew proposals for new tickets
// duplicating issues those narratives already named. The linked ids came out as a
// contiguous block (2-18), which is what identified it.
func TestMatch_NarrativeCapIsSeparateFromCandidateCap(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

	// 12 narratives, each with exactly one candidate so every match is
	// deterministic and no LLM call is needed.
	const total = 12
	for i := range total {
		key := "PAAS-40" + string(rune('0'+i/10)) + string(rune('0'+i%10))
		nid, err := s.InsertNarrative(
			base.Add(time.Duration(i)*time.Hour), base.Add(time.Duration(i+1)*time.Hour),
			"work on "+key, "summary")
		require.NoError(t, err)

		ext := "cap:" + key
		e := events.NewEvent("jira", ext, base.Add(time.Duration(i)*time.Hour),
			key+" status: Discovery → Ready for Dev")
		e.Artifacts = map[string]any{"issue_key": key, "connection": "dev"}
		_, err = s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID("jira", ext)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
	}

	// A tracker that resolves anything, so nothing is lost to verification.
	tracker := &anyKeyTracker{}
	client := &promptCapturingLLM{response: "[]"}

	// The old bug: a small candidate cap silently capped narratives too.
	results, _, err := correlator.Match(context.Background(), s, tracker, client,
		config.MatchConfig{
			MaxCandidatesPerNarrative: 2, // small on purpose
			ConfidenceFloor:           0.7,
		})

	require.NoError(t, err)
	assert.Len(t, results, total,
		"a small CANDIDATE cap must not shrink how many NARRATIVES a pass examines")

	linked, err := s.NarrativesWithNoIssueLink(100)
	require.NoError(t, err)
	assert.Empty(t, linked,
		"every narrative had one resolvable candidate, so none may be left untracked")
}

// TestMatch_NarrativeCapIsHonouredAndLogged: the cap still bounds a pass, and
// reaching it says so. Matching truncated silently before, which is why 20
// unexamined narratives read as a matching failure rather than a batch limit —
// Reconcile has logged its own cap since it shipped.
func TestMatch_NarrativeCapIsHonouredAndLogged(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

	const total = 7
	for i := range total {
		nid, err := s.InsertNarrative(
			base.Add(time.Duration(i)*time.Hour), base.Add(time.Duration(i+1)*time.Hour),
			"narrative", "summary")
		require.NoError(t, err)

		ext := "lim:" + string(rune('a'+i))
		e := events.NewEvent("jira", ext, base.Add(time.Duration(i)*time.Hour), "PAAS-1 status change")
		e.Artifacts = map[string]any{"issue_key": "PAAS-500" + string(rune('0'+i)), "connection": "dev"}
		_, err = s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID("jira", ext)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
	}

	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	results, _, err := correlator.Match(context.Background(), s, &anyKeyTracker{},
		&promptCapturingLLM{response: "[]"},
		config.MatchConfig{MaxNarrativesPerPass: 3, ConfidenceFloor: 0.7})

	require.NoError(t, err)
	assert.Len(t, results, 3, "the cap bounds the pass")
	assert.Contains(t, logged.String(), "match.max_narratives_per_pass",
		"the config key that would change this must be named")
	assert.Contains(t, logged.String(), "wait for the next pass",
		"silent truncation reads as a matching failure; say what happened")
}

// TestMatch_ExactlyFullBatchDoesNotWarn: fetching limit+1 is how Match tells
// "exactly full" from "more waiting". Without it a backlog matching the cap
// exactly would warn every pass, which trains a reader to ignore the line.
func TestMatch_ExactlyFullBatchDoesNotWarn(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

	const total = 3
	for i := range total {
		nid, err := s.InsertNarrative(
			base.Add(time.Duration(i)*time.Hour), base.Add(time.Duration(i+1)*time.Hour),
			"narrative", "summary")
		require.NoError(t, err)

		ext := "exact:" + string(rune('a'+i))
		e := events.NewEvent("jira", ext, base.Add(time.Duration(i)*time.Hour), "status change")
		e.Artifacts = map[string]any{"issue_key": "PAAS-600" + string(rune('0'+i)), "connection": "dev"}
		_, err = s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID("jira", ext)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
	}

	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	results, _, err := correlator.Match(context.Background(), s, &anyKeyTracker{},
		&promptCapturingLLM{response: "[]"},
		config.MatchConfig{MaxNarrativesPerPass: total, ConfidenceFloor: 0.7})

	require.NoError(t, err)
	assert.Len(t, results, total)
	assert.NotContains(t, logged.String(), "max_narratives_per_pass",
		"a backlog exactly at the cap has nothing waiting, so warning would be noise")
}

// anyKeyTracker resolves every key it is asked about.
type anyKeyTracker struct{}

func (anyKeyTracker) GetIssue(key string) (tasktracker.Issue, error) {
	return tasktracker.Issue{Key: key, StatusName: "Ready for Dev", Summary: key + " summary"}, nil
}

func (anyKeyTracker) AvailableTransitions(string) ([]tasktracker.Transition, error) {
	return nil, nil
}

func (anyKeyTracker) SearchIssues(string, int) ([]tasktracker.Issue, error) { return nil, nil }
