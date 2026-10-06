package reconciler

// destinations.go is the allowed-destination set (docs/superpowers/specs/
// 2026-10-06-tracker-model-design.md, "Destinations"): before any drafting call, where a
// narrative's work may be ticketed is computed deterministically, and a proposal outside
// that set is rejected. The three write gates still apply on top.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// DestinationPolicy answers where work may be ticketed. config.Config is the production
// policy; it is an interface so this package takes resolved answers rather than reading
// config itself, the same layering WithRules follows.
type DestinationPolicy interface {
	// UntrackedDestinations decides where untracked work done in repos may be ticketed.
	UntrackedDestinations(repos []string) config.DestinationPlan
	// IssueAcceptsWrites reports whether a linked issue is a destination for comments and
	// transitions at all, and why not.
	IssueAcceptsWrites(issueKey string) (bool, string)
}

// WithDestinations supplies the destination policy. Absent, Reconcile drafts for every
// actionable link and ProposeCreates proposes one create for the configured default,
// which is the behaviour before destinations existed.
func WithDestinations(policy DestinationPolicy) ReconcileOption {
	return func(o *reconcileOptions) {
		o.destinations = policy
	}
}

// workLocation is where a narrative's untracked work happened, in strict order: the
// repositories its transcripts' own SCM actions sent work to (events.ArtifactWorkRepos),
// because that names where the work WENT; else the remotes of the working copies it ran
// in (events.ArtifactCwdRemotes). Sorted and deduplicated. Empty means nothing is known.
func workLocation(evts []events.Event) []string {
	for _, key := range []string{events.ArtifactWorkRepos, events.ArtifactCwdRemotes} {
		var repos []string

		for _, e := range evts {
			for _, r := range events.ReposOf(e, key) {
				if !slices.Contains(repos, r) {
					repos = append(repos, r)
				}
			}
		}

		if len(repos) > 0 {
			slices.Sort(repos)

			return repos
		}
	}

	return nil
}

// writableLinks keeps the links that are destinations for comments and transitions,
// returning the reasons the rest are not.
func writableLinks(policy DestinationPolicy, links []store.NarrativeIssue) ([]store.NarrativeIssue, []string) {
	if policy == nil {
		return links, nil
	}

	var (
		kept    []store.NarrativeIssue
		reasons []string
	)

	for _, l := range links {
		if ok, reason := policy.IssueAcceptsWrites(l.IssueKey); ok {
			kept = append(kept, l)
		} else {
			reasons = append(reasons, reason)
		}
	}

	return kept, reasons
}

// openCreateScopes is every scope an open or applied create of this narrative already
// targets: proposing another there would double the queue or open a duplicate. A create
// recorded before destinations existed names no scope, and blocks every scope, since
// which one it targets is unknown.
func openCreateScopes(actions []store.ActionRow) (scopes []string, blocksAll bool) {
	for _, a := range actions {
		if a.Type != string(ActionCreate) {
			continue
		}

		if a.Status != store.StatusApplied && a.Status != StatusProposed && a.Status != store.StatusApproved {
			continue
		}

		scope := CreateScopeOf(a.Payload)
		if scope == "" {
			return nil, true
		}

		scopes = append(scopes, scope)
	}

	return scopes, false
}

// chooseDestinations returns the destinations the model named, and a suppression for any
// it named outside the allowed set. With one allowed destination the model is not asked,
// and that destination is the answer.
func chooseDestinations(allowed []config.Destination, named []string) ([]config.Destination, []string) {
	if len(allowed) == 1 {
		return allowed, nil
	}

	var (
		chosen   []config.Destination
		rejected []string
	)

	for _, name := range named {
		i := slices.IndexFunc(allowed, func(d config.Destination) bool { return d.Tracker == name })
		if i < 0 {
			rejected = append(rejected, fmt.Sprintf("%q", name))

			continue
		}

		if !slices.Contains(chosen, allowed[i]) {
			chosen = append(chosen, allowed[i])
		}
	}

	var suppressed []string

	if len(rejected) > 0 {
		suppressed = append(suppressed, fmt.Sprintf(
			"the proposal named %s, outside the allowed destinations (%s): rejected",
			strings.Join(rejected, ", "), destinationNames(allowed)))
	}

	if len(chosen) == 0 && len(rejected) == 0 {
		suppressed = append(suppressed, fmt.Sprintf(
			"the proposal named no destination, and several are allowed (%s): nothing proposed",
			destinationNames(allowed)))
	}

	return chosen, suppressed
}

// destinationNames renders allowed destinations for a prompt or a reason.
func destinationNames(ds []config.Destination) string {
	names := make([]string, len(ds))
	for i, d := range ds {
		names[i] = fmt.Sprintf("%q", d.Tracker)
	}

	return strings.Join(names, ", ")
}
