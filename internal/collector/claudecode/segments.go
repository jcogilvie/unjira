package claudecode

// segments.go splits one transcript into contiguous branch runs.
//
// Finding F15: a session was collapsed into a single event dated to its LAST message.
// Session e951ef78 ran 71 days across `main` → `vp-feedback-refactor` →
// `mesh-routing-learnings`; the middle run ended 2026-07-09, the same second as the
// merge PAAS-3905 retro-credits, and the one event unjira produced was dated a week
// later on the wrong branch. Every date comparison in the pipeline — the reconciler's
// delta, NarrativesOverlapping, any --since window — was therefore against "when did
// you last type" rather than "when was the work".
//
// Branch is the split signal because a branch name is an explicit human act of naming
// the work, which is why ProvenanceBranch ranks second only to a Jira event. cwd is
// NOT: it changes mid-session in 5 of 164 transcripts and 4 of those are a parent repo
// delegating to its own worktree — orchestration of one task. The single genuine focus
// change also changed branch, so splitting on cwd would fragment a task and buy nothing.

import (
	"slices"

	"github.com/jcogilvie/unjira/internal/events"
)

// segment is one contiguous run of transcript lines on a single branch — the unit that
// becomes an event.
type segment struct {
	gitBranch, cwd  string
	firstTS, lastTS string
	userTexts       []string
	orderedKeys     []string
	// factLines are the raw transcript lines this run covered, kept only so
	// sessionFacts can be computed ONCE over the whole run and ordered by factRules
	// (finding F25). Accumulating phrases per line instead would fix them in
	// observation order, which is exactly the churn the ordering exists to prevent.
	//
	// Scoped to the run for the same reason keys are: a commit made on one branch is
	// not evidence about another segment's work.
	factLines []map[string]any
	// scmKeys are keys this run named while AUTHORING in the SCM — a commit message,
	// a branch creation, a PR title (finding F20).
	//
	// Kept separate from orderedKeys rather than merged into it, because the two carry
	// different evidential strength and the correlator ranks on exactly that: a prose
	// mention is someone talking about a ticket, while a commit message is someone
	// naming the ticket for this work. Merging would flatten that distinction the way
	// ArtifactTicketKeys already flattens branch-vs-prose — the loss F15's comment
	// calls out.
	scmKeys []string
	// allBranches is every branch the WHOLE session touched, in first-seen order,
	// copied onto each segment.
	//
	// Deliberately the full set rather than this segment's own branch. Slicing helps
	// only sessions that change branch, and 42 of 79 multi-day sessions never do — so
	// the set is what gives the correlator something to weigh in the other half of the
	// cases. Deciding whether three branches are one story is judgment, and judgment
	// cannot weigh what it is not shown.
	allBranches []string
}

// segments walks a transcript once and returns its contiguous branch runs, merging runs
// shorter than minMessages into their neighbour.
//
// The floor exists because raw slicing over-splits. Measured across 46 multi-branch
// sessions, 30 produced more contiguous runs than distinct branches — one had 3 branches
// and 51 runs. Our own session showed a 3-minute, 16-line `rebase-probe` detour sitting
// between two halves of one 435-line body of work; emitting that as a peer would shred a
// session rather than disentangle it.
//
// A below-floor run FOLDS into its neighbour rather than being dropped: its messages and
// ticket keys carry over. Discarding them would be silent data loss, which this codebase
// errors over rather than accepts.
//
// minMessages <= 1 disables folding, which is what the pure split-boundary tests want.
func segments(lines []map[string]any, minMessages int) []segment {
	raw, allBranches := rawRuns(lines)
	if len(raw) == 0 {
		return nil
	}

	merged := foldShortRuns(raw, minMessages)

	for i := range merged {
		merged[i].allBranches = allBranches
	}

	return merged
}

// rawRuns produces one run per branch change, plus every branch the session touched.
//
// A line with no branch extends whatever run is open rather than starting a new one:
// transcripts interleave system and summary lines that carry no gitBranch, and treating
// each as a boundary would fragment on noise.
func rawRuns(lines []map[string]any) (runs []segment, allBranches []string) {
	seen := map[string]bool{}
	var cur *segment

	for _, line := range lines {
		branch, _ := line["gitBranch"].(string)
		cwd, _ := line["cwd"].(string)
		ts, _ := line["timestamp"].(string)
		text := messageText(line)
		isUser := line["type"] == lineTypeUser

		if branch != "" && !seen[branch] {
			seen[branch] = true
			allBranches = append(allBranches, branch)
		}

		// Start a new run only on an actual change to a NAMED branch.
		if branch != "" && (cur == nil || cur.gitBranch != branch) {
			if cur != nil {
				runs = append(runs, *cur)
			}
			cur = &segment{gitBranch: branch}
		}

		if cur == nil {
			// Lines before any branch is known: only worth keeping if they carry
			// timestamps or text, which the accumulate below handles once a run opens.
			if ts == "" && text == "" {
				continue
			}
			cur = &segment{}
		}

		accumulate(cur, cwd, ts, text, isUser)

		// SCM keys come from the line's TOOL CALLS, which messageText discards — so
		// they are gathered separately rather than via text (finding F20). Scoped to
		// the open run for the same reason prose keys are: a key committed on one
		// branch must not become a candidate for another segment's work.
		for _, key := range scmKeys(line) {
			if !slices.Contains(cur.scmKeys, key) {
				cur.scmKeys = append(cur.scmKeys, key)
			}
		}

		// What the run DID, from the same tool calls (finding F25). Accumulated per line
		// and ordered once at the end rather than here: sessionFacts sorts into
		// factRules order, and appending per line would leave observation order instead.
		cur.factLines = append(cur.factLines, line)
	}

	if cur != nil {
		runs = append(runs, *cur)
	}

	return runs, allBranches
}

// accumulate folds one line's data into an open run.
func accumulate(seg *segment, cwd, ts, text string, isUser bool) {
	if cwd != "" {
		seg.cwd = cwd
	}
	if ts != "" {
		if seg.firstTS == "" {
			seg.firstTS = ts
		}
		seg.lastTS = ts
	}
	if text == "" {
		return
	}

	// Keys are scoped to the run that mentioned them. A key named only while on one
	// branch must not become a candidate for another segment's work — gatherCandidates
	// treats a prose mention as a real candidate, so leaking them sideways would
	// manufacture links from work that never referenced the ticket.
	for _, key := range events.ExtractTicketKeys(text) {
		if !slices.Contains(seg.orderedKeys, key) {
			seg.orderedKeys = append(seg.orderedKeys, key)
		}
	}

	if isUser {
		seg.userTexts = append(seg.userTexts, text)
	}
}

// foldShortRuns merges runs below the floor into a neighbour, then coalesces runs that
// have become adjacent on the same branch.
//
// Two passes rather than one, because folding is what CREATES the adjacency: dropping the
// `rebase-probe` detour is what leaves two `feature` runs next to each other, and only
// then can they merge.
func foldShortRuns(runs []segment, minMessages int) []segment {
	if minMessages > 1 {
		runs = dropBelowFloor(runs, minMessages)
	}

	return coalesceAdjacent(runs)
}

// dropBelowFloor folds every run with too few user messages into the run before it, or
// after it when it is first.
func dropBelowFloor(runs []segment, minMessages int) []segment {
	out := make([]segment, 0, len(runs))

	for _, run := range runs {
		if len(run.userTexts) >= minMessages || len(runs) == 1 {
			out = append(out, run)

			continue
		}

		if len(out) > 0 {
			absorb(&out[len(out)-1], run)

			continue
		}

		// First run is below the floor: keep it open so the NEXT run absorbs it,
		// which preserves its messages while letting the larger run name the segment.
		out = append(out, run)
	}

	return out
}

// coalesceAdjacent merges neighbouring runs that share a branch.
func coalesceAdjacent(runs []segment) []segment {
	out := make([]segment, 0, len(runs))

	for _, run := range runs {
		if len(out) > 0 && sameOrUnnamed(out[len(out)-1], run) {
			prev := &out[len(out)-1]
			if prev.gitBranch == "" {
				prev.gitBranch = run.gitBranch
			}
			absorb(prev, run)

			continue
		}

		out = append(out, run)
	}

	return out
}

// sameOrUnnamed reports whether b continues a: the same branch, or either side unnamed
// (a run of branch-less lines belongs to whatever it sits inside).
func sameOrUnnamed(a, b segment) bool {
	return a.gitBranch == b.gitBranch || a.gitBranch == "" || b.gitBranch == ""
}

// absorb folds src into dst, extending the interval and preserving every message and key.
func absorb(dst *segment, src segment) {
	if dst.firstTS == "" || (src.firstTS != "" && src.firstTS < dst.firstTS) {
		dst.firstTS = src.firstTS
	}
	if src.lastTS > dst.lastTS {
		dst.lastTS = src.lastTS
	}
	if src.cwd != "" {
		dst.cwd = src.cwd
	}

	dst.userTexts = append(dst.userTexts, src.userTexts...)
	for _, key := range src.orderedKeys {
		if !slices.Contains(dst.orderedKeys, key) {
			dst.orderedKeys = append(dst.orderedKeys, key)
		}
	}
	// SCM keys merge too, and must: a below-floor run that did nothing but commit is
	// exactly the run whose key matters most, and dropping it here would be silent
	// data loss in the one direction F20 exists to fix.
	for _, key := range src.scmKeys {
		if !slices.Contains(dst.scmKeys, key) {
			dst.scmKeys = append(dst.scmKeys, key)
		}
	}
	// And the lines the facts are derived from, for the same reason: a folded run that
	// only ran `git commit` carries the one fact the merged segment most needs.
	dst.factLines = append(dst.factLines, src.factLines...)
}
