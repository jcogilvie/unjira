package pipeline

// context_narratives.go bounds how many EXISTING narratives one narration pass
// hydrates as clustering context — F16's other lever, config.CorrelatorConfig.
// MaxContextNarratives.
//
// WHY THIS IS THE RIGHT LEVER. Re-measured across nine dev narrate --dry-run runs
// (docs/architecture-findings.md, F16): completion tokens track EXTENDS-cluster
// count, and EXTENDS count equals the context-narrative count in every run — 32
// in, 32 out, every time. Neither the per-event summary cap
// (correlator.WithMaxEventSummaryChars) nor a coarser grouping instruction moves
// that ratio, because it is a property of what the prompt is GIVEN — one existing
// narrative, one opportunity to extend it — not of how the prompt asks for it.
//
// WHY A BARE LIMIT IS THE WRONG SHAPE. store.NarrativesOverlapping orders
// (window_start, id) ascending, so LIMIT keeps the OLDEST rows — close to the
// worst possible choice: the newest overlapping narrative is the likeliest to be
// extended by a new event, and a story worked on yesterday is likelier to
// continue than one from three weeks ago. Worse than the token cost, dropping the
// WRONG narrative corrupts the data it was meant to protect: the model cannot see
// the story an event belongs to, so it opens a spurious "new" cluster and
// fragments a narrative that already exists.
//
// THE ORDERING. selectContextNarratives keeps two tiers, in order:
//
//  1. Any narrative already linked (any narrative_issues role) to an issue key
//     this window's own candidate events also name. This is the strongest signal
//     available without a model call: match_candidates.go's gatherCandidates
//     already ranks candidate provenance for exactly this reason (a branch name or
//     an SCM-authored commit is an explicit human act of naming a ticket), and a
//     narrative ABOUT the same ticket a new event names is the single most likely
//     thing that event extends.
//  2. Everything else, by most recent window_end first — a cheap, always-available
//     recency signal requiring no store round trip Match hasn't already made.
//
// WHAT THIS LOSES. A narrative that shares no issue key with the window (because
// neither side has matched one yet, or because the connecting thread is a
// branch/topic rather than a ticket) and is not among the most recent ranks below
// both tiers and can be dropped — the same class of loss any bound accepts, stated
// so an operator can weigh it. Recorded in docs/architecture-findings.md F16 as
// available-but-UNMEASURED: this repo's design-notes #37/#38 record why a
// worktree with no Jira credentials cannot produce a trustworthy before/after
// token number, so none is claimed here.
//
// Exclusions are REPORTED (NarrateResult.ExcludedContextNarratives), mirroring
// ExcludedTrackerRecords: an unreported exclusion reads as "nothing was left
// out", and this is a genuinely unmeasured knob, so an operator needs the count
// to tune it against evidence rather than guessing blind.

import (
	"sort"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// candidateIssueKeys returns the set of issue keys named anywhere across evts —
// every provenance tier match_candidates.go's gatherCandidates recognizes,
// flattened to a set: a Jira-sourced event's own issue key, a git branch name, an
// SCM-authoring artifact, and free-text mentions. Ranking narrative relevance only
// needs "does this window touch this key at all", not gatherCandidates' full
// provenance ordering — that ordering exists to pick ONE key to verify against the
// tracker, a different question from "which stories might this window extend".
//
// Never nil: a caller ranging over it, or comparing membership, should not need a
// nil check for "no candidates in this window" — the same convention
// TicketKeysOf/SCMKeysOf degrade to for a missing artifact.
func candidateIssueKeys(evts []events.Event) map[string]bool {
	out := make(map[string]bool)

	add := func(key string) {
		if key != "" {
			out[key] = true
		}
	}

	for _, e := range evts {
		if issueKey, ok := e.Artifacts[events.ArtifactIssueKey].(string); ok {
			add(issueKey)
		}
		if branch, ok := e.Artifacts[events.ArtifactGitBranch].(string); ok && branch != "" {
			for _, key := range events.ExtractTicketKeys(branch) {
				add(key)
			}
		}
		for _, key := range events.SCMKeysOf(e) {
			add(key)
		}
		for _, key := range events.TicketKeysOf(e) {
			add(key)
		}
	}

	return out
}

// selectContextNarratives bounds rows to at most maxContext, returning the kept
// rows (in ranked order — see the file doc comment for the two-tier ordering) and
// how many were excluded. maxContext <= 0 means unlimited: rows pass through
// unchanged, in NarrativesOverlapping's own (window_start, id) order, so every
// existing caller (today, none — this is new) means exactly what it always meant.
//
// linkedKeys is narrative id -> its narrative_issues keys (store.
// NarrativeIssueKeysByNarrative; a missing id means "no links yet", not "linked to
// nothing" — see that function's own doc comment). candidateKeys is the window's
// own gathered keys (candidateIssueKeys, above). Either may be nil: an unmatched
// store or a window with no key-bearing events are both ordinary states, not
// errors, and ranking degrades to recency-only exactly as if every row were
// unlinked.
//
// A pure function deliberately: ranking WHICH narratives are relevant is a
// deterministic pre-filter (CLAUDE.md's stated pattern — see gatherCandidates and
// runSuppression for the other two), and belongs in Go. Deciding what a numbered
// event actually extends stays the model's job in correlator.Cluster.
func selectContextNarratives(
	rows []store.NarrativeRow, linkedKeys map[int64][]string, candidateKeys map[string]bool, maxContext int,
) ([]store.NarrativeRow, int) {
	if maxContext <= 0 || len(rows) <= maxContext {
		return rows, 0
	}

	sharesKey := func(id int64) bool {
		for _, key := range linkedKeys[id] {
			if candidateKeys[key] {
				return true
			}
		}

		return false
	}

	ranked := make([]store.NarrativeRow, len(rows))
	copy(ranked, rows)

	sort.SliceStable(ranked, func(i, j int) bool {
		si, sj := sharesKey(ranked[i].ID), sharesKey(ranked[j].ID)
		if si != sj {
			return si // a shared-key row always outranks one that doesn't share
		}
		if !ranked[i].WindowEnd.Equal(ranked[j].WindowEnd) {
			return ranked[i].WindowEnd.After(ranked[j].WindowEnd) // most-recent-first within a tier
		}

		// Deterministic tiebreak, matching match_candidates.go's rankCandidates: an
		// unstable order would let an otherwise-identical pass select a different
		// context set from one run to the next.
		return ranked[i].ID < ranked[j].ID
	})

	kept := ranked[:maxContext]

	return kept, len(rows) - len(kept)
}
