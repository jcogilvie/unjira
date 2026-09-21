package github

// events.go builds the events this collector emits from a PR and (when it has
// left the open state) its issue-events timeline. Pure functions — no
// network, no store — mirroring collector/jira/events.go's split from
// jira.go's I/O.
//
// Two event kinds, per
// docs/superpowers/specs/2026-09-17-github-collector-design.md:
//   - :opened, captured once at PR creation.
//   - :merged / :closed, one per matching timeline entry — see
//     CompletionEvents' own doc comment for why a merge legitimately produces
//     BOTH a :merged and a :closed event, and why that is correct rather than
//     a bug to suppress.

import (
	"fmt"
	"strings"

	ghclient "github.com/jcogilvie/unjira/internal/clients/github"
	"github.com/jcogilvie/unjira/internal/events"
)

// artifactCompletionKind is collector-private (not in events/artifact_keys.go
// — per docs/go-conventions.md, "only keys read outside their producing
// collector belong [in the shared vocabulary]", and nothing outside this
// collector reads it yet). It records which of "merged" or "closed" one
// completion event represents, so a future consumer can read the
// dispositiveness distinction the reviewer's decision 2 calls for — a merge
// is strong evidence work completed, a closed-unmerged PR is evidence work
// happened without completing — without re-deriving it by parsing the
// ExternalID's suffix.
const artifactCompletionKind = "completion_kind"

// completionOpened, completionMerged, completionClosed name the ExternalID
// segment and, for the latter two, the artifactCompletionKind value. GitHub's
// own timeline vocabulary ("merged", "closed") is reused verbatim rather than
// renamed, since these are collector-private strings nothing outside this
// package interprets.
const (
	completionOpened = "opened"
	completionMerged = "merged"
	completionClosed = "closed"
)

// prSummary renders the shared "<owner>/<repo>#<N>: <title>\n\n<body>" text
// both event kinds carry, mirroring EventFromIssueBody's
// "<KEY>: <summary>\n\n<body>" shape. Body is omitted entirely when empty,
// matching EventFromIssueBody's own trimming: an absent body must not read as
// a two-newline gap.
func prSummary(ref ghclient.RepoRef, pr ghclient.PullRequest) string {
	head := fmt.Sprintf("%s#%d: %s", ref.OwnerRepo(), pr.Number, pr.Title)

	body := strings.TrimSpace(pr.Body)
	if body == "" {
		return head
	}

	return head + "\n\n" + body
}

// annotate sets the artifacts both event kinds share: the head branch (reused
// verbatim as events.ArtifactGitBranch, the identical signal
// collector/claudecode already writes — see the design's §2), and the ticket
// keys named in the PR's title/body (events.ArtifactSCMKeys,
// ProvenanceSCMCommand's tier — a PR title/body is, by construction, an
// authoring artifact: nobody opens a PR to investigate, only to propose a
// change).
//
// Deliberately never calls events.SetTrackerRecord: a pull request is a thing
// somebody did, work evidence in every deployment, never the tracker's own
// account of itself in the deployments this slice supports (see the design's
// §"Which side of the diff GitHub sits on"). Deliberately never sets
// events.ArtifactIssueKey either — that artifact means "this event is already
// about that issue" (ProvenanceJiraEvent's tier), and a PR merely mentioning a
// key is a weaker, inferred signal that belongs on ArtifactSCMKeys/
// ArtifactGitBranch instead.
func annotate(evt *events.Event, pr ghclient.PullRequest) {
	evt.Actor = pr.User.Login
	evt.RawRef = pr.HTMLURL
	evt.Artifacts[events.ArtifactGitBranch] = pr.Head.Ref
	events.SetSCMKeys(evt, events.ExtractTicketKeys(pr.Title+" "+pr.Body))
}

// OpenedEvent builds the one-time :opened event for pr. Captured once, at (or
// shortly after) PR creation: GitHub's created_at does not change once set,
// and this collector does not attempt artifact re-derivation on a later edit
// (see F21 — that is a separate, deferred mechanism this collector would use
// unmodified if built).
func OpenedEvent(ref ghclient.RepoRef, pr ghclient.PullRequest) events.Event {
	evt := events.NewEvent(
		Name,
		fmt.Sprintf("%s#%d:%s", ref.OwnerRepo(), pr.Number, completionOpened),
		pr.CreatedAt,
		prSummary(ref, pr),
	)

	annotate(&evt, pr)

	return evt
}

// CompletionEvents builds every :merged/:closed event for pr from its own
// issue-events timeline, called only once pr has left the open state.
//
// ONE EVENT PER MATCHING TIMELINE ENTRY, not one event per PR. A merge
// genuinely emits two timeline entries — a "merged" and a "closed" event,
// independently timestamped with their own immutable ids (measured live,
// PR #69: merged id=31343430711 at T, closed id=31343430804 93ms later) — and
// both are emitted here as distinct events with distinct ExternalIDs.
// Deduplicating them (treating a "merged" as suppressing its paired "closed")
// was considered and rejected: CLAUDE.md's "collectors are dumb and
// deterministic" puts that judgment in the reconciler, not the collector, and
// GitHub genuinely recorded two distinct state transitions with two distinct
// ids — collapsing them would be this collector asserting an interpretation.
// A narrative holding both simply has two true facts about one PR (a merge
// does close the PR), which downstream consumers can weigh using
// artifactCompletionKind without re-deriving it.
//
// Keying on the timeline entry's own id, rather than a fixed
// "<owner>/<repo>#N:closed" per PR, is what fixes the reopened-PR collision:
// a PR that is closed, reopened, and re-closed produces a SECOND :closed
// event with a distinct id-suffixed ExternalID rather than colliding with (and
// being silently dropped by INSERT OR IGNORE alongside) the first.
//
// Errors rather than returning zero events when pr.IsClosed() but no
// merged/closed timeline entry matches — "never silently drop data": a PR
// that demonstrably left the open state must account for why, and a quiet
// empty result would look identical to "nothing happened yet".
func CompletionEvents(ref ghclient.RepoRef, pr ghclient.PullRequest, timeline []ghclient.IssueEvent) ([]events.Event, error) {
	if !pr.IsClosed() {
		return nil, nil
	}

	var out []events.Event

	for _, entry := range timeline {
		var kind string

		switch entry.Event {
		case completionMerged:
			kind = completionMerged
		case completionClosed:
			kind = completionClosed
		default:
			continue
		}

		if entry.ID == 0 {
			return nil, fmt.Errorf(
				"%s#%d: %s timeline entry has no usable id", ref.OwnerRepo(), pr.Number, kind,
			)
		}

		evt := events.NewEvent(
			Name,
			fmt.Sprintf("%s#%d:%s:%d", ref.OwnerRepo(), pr.Number, kind, entry.ID),
			entry.CreatedAt,
			prSummary(ref, pr),
		)
		evt.Artifacts[artifactCompletionKind] = kind
		annotate(&evt, pr)

		out = append(out, evt)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf(
			"%s#%d is closed but its issue-events timeline holds no merged/closed entry: "+
				"refusing to silently record zero completion events for work that demonstrably finished",
			ref.OwnerRepo(), pr.Number,
		)
	}

	return out, nil
}
