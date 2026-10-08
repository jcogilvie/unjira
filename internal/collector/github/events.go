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
// keys the PR names.
//
// The title and the body are different acts, so their keys land on different
// artifacts. The title is where the author names the ticket for the change, the
// same act as a commit subject, so its keys are events.ArtifactSCMKeys
// (ProvenanceSCMCommand's tier). The body is prose about the change, and prose cites:
// related tickets, the ticket a bug was found under, keys quoted as examples. Its keys
// are events.ArtifactTicketKeys, the prose tiers, so a body citation is a candidate
// the model judges, not an authoring fact. Keys inside Markdown code in the body are
// not candidates at all (events.BlankMarkdownCode): code is the author quoting text
// as data. On a real 30-day store, title and body were one SCM-tier list, and 12
// narratives about this repository's own pull requests took a key quoted in a body
// as their primary. Ten were the lone candidate, so were linked at confidence 1.0
// with no model call, and each drew a comment proposal on a ticket the work never
// touched. On the same store, every pull request in the other repositories that
// named a ticket named it in the title.
//
// Deliberately never calls events.SetTrackerRecord: a pull request is a thing
// somebody did, work evidence in every deployment, never the tracker's own
// account of itself in the deployments this slice supports (see the design's
// §"Which side of the diff GitHub sits on"). Deliberately never sets
// events.ArtifactIssueKey either — that artifact means "this event is already
// about that issue" (ProvenanceJiraEvent's tier), and a PR merely mentioning a
// key is a weaker, inferred signal that belongs on ArtifactSCMKeys/
// ArtifactGitBranch instead.
//
// It also sets events.ArtifactPullRequest, the PR's host-qualified identity
// (events.PullRequestRef). The claudecode collector writes the same value on an anchor
// whose `gh pr create` result named this PR, so the two sides of one fact share an
// identifier the clustering pre-filter joins on (pipeline's PR-identity
// pre-assignment) without parsing an ExternalID. The host is the configured repo's,
// so a GHES PR never joins a github.com PR with the same owner/repo#N. ExternalIDs and
// summaries keep the host-less "<owner>/<repo>#<N>" they always had.
//
// And events.ArtifactWorkRepos, the PR's repository: where the work went, exactly, in
// the host-qualified, case-folded form events.NormalizeRepo writes for the claudecode
// collector's work locations. Without it a narrative made only of PR events has no
// location, and destinations send it to default_ticket_in whatever repository it was
// in.
func annotate(evt *events.Event, ref ghclient.RepoRef, pr ghclient.PullRequest) {
	evt.Actor = pr.User.Login
	evt.RawRef = pr.HTMLURL
	evt.Artifacts[events.ArtifactPullRequest] = events.PullRequestRef(ref.Host, ref.Owner, ref.Repo, pr.Number)
	events.SetRepos(evt, events.ArtifactWorkRepos, []string{strings.ToLower(ref.Host + "/" + ref.Owner + "/" + ref.Repo)})
	evt.Artifacts[events.ArtifactGitBranch] = pr.Head.Ref
	events.SetSCMKeys(evt, events.ExtractTicketKeys(pr.Title))
	events.SetTicketKeys(evt, events.ExtractTicketKeys(events.BlankMarkdownCode(pr.Body)))
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

	annotate(&evt, ref, pr)

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
		annotate(&evt, ref, pr)

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
