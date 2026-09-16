package claudecode

// facts_test.go covers finding F25: an event's summary named its own message count
// while withholding the messages, so any reconciler rationale of the form "no
// evidence of X" was unfalsifiable rather than wrong.
//
// The measured case, found by triaging a real proposal: a transition was rationalised
// as "only 3 messages logged with no PR or completion evidence yet" about a session
// that had committed AND edited a PR. Across the store, 388 of 419 events (93%) omit
// content and the worst drops 376 of 377 messages.
//
// The fix is deliberately NOT "include every message" — userTexts for a 377-message
// session is exactly the volume F16 attributes 93.7% of prompt cost to. Instead a
// bounded set of DETERMINISTIC facts, extracted the way scmKeys already extracts keys:
// what the session did, in a fixed number of words, with no model involved.

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// factLine builds a transcript line carrying one Bash tool call.
func factLine(cmd string) map[string]any { return factLineOn("f", cmd) }

// factLineOn is factLine on a named branch, so an integration test can keep its tool
// calls inside the same segment as its user messages rather than splitting a run.
func factLineOn(branch, cmd string) map[string]any {
	return map[string]any{
		"timestamp": "2026-09-01T10:00:00Z", "type": "assistant",
		"gitBranch": branch, "cwd": "/w/u",
		"message": map[string]any{"content": []any{
			map[string]any{
				"type": "tool_use", "name": "Bash",
				"input": map[string]any{"command": cmd},
			},
		}},
	}
}

func TestSessionFacts_DetectsAuthoring(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, want string
	}{
		{"commit", `git commit -m "PROJ-1: the change"`, "committed"},
		{"branch create", `git checkout -b proj-1-work`, "created a branch"},
		{"switch -c", `git switch -c proj-1-work`, "created a branch"},
		{"pr create", `gh pr create --title "PROJ-1: x"`, "opened a PR"},
		{"pr edit", `gh pr edit 42 --body "updated"`, "updated a PR"},
		{"rtk commit", `rtk git commit -m "PROJ-1: x"`, "committed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Contains(t, sessionFacts([]map[string]any{factLine(tc.cmd)}), tc.want)
		})
	}
}

func TestSessionFacts_DetectsTestRuns(t *testing.T) {
	for _, cmd := range []string{
		`helm unittest -f "tests/x_test.yaml" .`,
		`go test ./...`,
		`./test.sh`,
		`make e2e`,
	} {
		assert.Contains(t, sessionFacts([]map[string]any{factLine(cmd)}), "ran tests",
			"command %q should read as a test run", cmd)
	}
}

// TestSessionFacts_IgnoresReadingCommands mirrors scmKeys' distinction. Inspecting a
// PR is not evidence of having opened one, and `git log` is not evidence of a commit.
// This is the same 8-of-15-keys measurement that shaped F20's extraction.
func TestSessionFacts_IgnoresReadingCommands(t *testing.T) {
	for _, cmd := range []string{
		`git log --oneline -5`,
		`git show HEAD`,
		`gh pr view 42`,
		`git diff main..feature`,
		`git status`,
	} {
		assert.Empty(t, sessionFacts([]map[string]any{factLine(cmd)}),
			"reading command %q must not read as authorship", cmd)
	}
}

// TestSessionFacts_IsBoundedAndDeduplicated is the F16 guard. The whole point is a
// FIXED-size addition to the summary — a session that commits forty times must add
// the same handful of words as one that commits once.
func TestSessionFacts_IsBoundedAndDeduplicated(t *testing.T) {
	lines := make([]map[string]any, 0, 40)
	for range 40 {
		lines = append(lines, factLine(`git commit -m "PROJ-1: again"`))
	}

	got := sessionFacts(lines)

	assert.Equal(t, []string{"committed"}, got,
		"repeated evidence of the same fact must collapse: the summary's size cannot scale with "+
			"session length, or this undoes F16 at the source")
}

// TestSessionFacts_OrderIsStable keeps the summary from churning. An event's summary
// is part of its identity for a reader diffing two passes, so the same session must
// always render the same fact order.
func TestSessionFacts_OrderIsStable(t *testing.T) {
	lines := []map[string]any{
		factLine(`go test ./...`),
		factLine(`gh pr create --title "x"`),
		factLine(`git commit -m "y"`),
	}

	first := sessionFacts(lines)
	second := sessionFacts(lines)

	assert.Equal(t, first, second)
	assert.Equal(t, []string{"committed", "opened a PR", "ran tests"}, first,
		"a declared order, not observation order, so two sessions doing the same things read alike")
}

// TestSessionFacts_DegenerateInputs keeps callers from needing a guard.
func TestSessionFacts_DegenerateInputs(t *testing.T) {
	assert.Empty(t, sessionFacts(nil))
	assert.Empty(t, sessionFacts([]map[string]any{}))
	assert.Empty(t, sessionFacts([]map[string]any{{"type": "user"}}))
	assert.Empty(t, sessionFacts([]map[string]any{factLine("")}))
}

// TestSegmentSummary_StatesWhatTheSessionDid is the finding's actual fix. The
// motivating case: a session that committed and edited a PR was summarised as though
// there were no evidence of either.
func TestSegmentSummary_StatesWhatTheSessionDid(t *testing.T) {
	lines := []map[string]any{
		line("2026-09-01T10:00:00Z", "vpc-output-application", "/w/u", "user",
			"let's get the observed-infra Application into xvpc's outputs"),
		factLineOn("vpc-output-application", `git commit -m "PROJ-1: emit observed-infra Application"`),
		factLineOn("vpc-output-application", `gh pr edit 90210 --body "PROJ-1: emits an Application"`),
		line("2026-09-01T10:30:00Z", "vpc-output-application", "/w/u", "user",
			"rebase on main and leave it for review"),
	}

	segs := segments(lines, 0)
	got := segmentSummary("helm-charts", segs[0])

	assert.Contains(t, got, "committed",
		"a rationale must not be able to say 'no completion evidence' about a session that committed")
	assert.Contains(t, got, "updated a PR")
	assert.Contains(t, got, "2 user messages", "the count stays: it is still true")
	assert.Contains(t, got, "observed-infra Application", "and the opening line stays")
}

// TestSegmentSummary_UnchangedWhenNothingWasDone pins that the addition is additive.
// A session that only talked reads exactly as it did before, so 31 of 419 events —
// the ones whose summary was already complete — are untouched.
func TestSegmentSummary_UnchangedWhenNothingWasDone(t *testing.T) {
	lines := []map[string]any{
		line("2026-09-01T10:00:00Z", "feature", "/w/u", "user", "what do you think about this design?"),
	}

	segs := segments(lines, 0)
	got := segmentSummary("helm-charts", segs[0])

	assert.NotContains(t, got, "committed")
	assert.NotContains(t, got, "Did:",
		"no facts means no facts clause at all, rather than an empty one that reads as 'did nothing'")
}
