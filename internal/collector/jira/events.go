// Package jira collects Jira changes into the event log, so the correlator can
// see what the org believes happened alongside what the transcripts show.
//
// It emits four event kinds — status transitions, comments, and description and
// summary edits. Every other changelog field is deliberately skipped: phase 1
// can only propose comments and forward transitions, so an event about a label
// or sprint change is something the correlator must reason about and can never
// act on. A groomed backlog generates constant churn in those fields, and
// emitting it would bury the signal. See
// docs/superpowers/specs/2026-08-21-jira-collector-design.md.
package jira

import (
	"fmt"
	"time"

	"github.com/jcogilvie/unjira/internal/events"
)

// Name is the collector's registry key and the Source on every event it emits.
const Name = "jira"

// jiraTimeFormat is Jira's changelog/comment timestamp layout: ISO 8601 with
// milliseconds and a numeric zone offset with no colon, which is why
// time.RFC3339 cannot parse it.
const jiraTimeFormat = "2006-01-02T15:04:05.000-0700"

// fieldStatus is Jira's changelog field name for a status transition. Named
// separately (rather than the literal repeated below) because it also drives
// the special-cased "from → to" summary format status transitions get.
const fieldStatus = "status"

// trackedFields are the changelog fields that become events, mapped to the
// ExternalID segment naming them. A map rather than a slice so lookup is the
// same operation as the skip decision.
//
// Jira reports custom field names with their configured display name (Sprint,
// Story Points), so an unlisted field simply falls through — which is the
// intent: this list grows when an action type needs it.
var trackedFields = map[string]string{
	fieldStatus:   fieldStatus,
	"description": "description",
	"summary":     "summary",
}

// IssueContext is the per-issue data every event from that issue carries. It is
// passed rather than re-derived so the pure event builders need no client.
type IssueContext struct {
	Key        string
	ProjectKey string
	// Connection is the configured connection name, recorded so the reconciler
	// knows which Jira site to write back to.
	Connection string
	Site       string
	// SelfAccountID is our own Jira accountId, from one Myself() call per pass.
	// Empty means "unknown", in which case nothing is tagged self-authored.
	SelfAccountID string
}

// browseURL is the human-facing link back to the issue.
func (ic IssueContext) browseURL() string {
	return fmt.Sprintf("%s/browse/%s", ic.Site, ic.Key)
}

// EventsFromChangelogEntry converts one changelog entry into zero or more
// events — zero when it touched only untracked fields, more than one when Jira
// batched several tracked changes into a single entry (it does: a transition
// that also edits the summary is one entry with two items).
//
// Returns an error rather than skipping when the entry's shape is unreadable: a
// silently dropped entry is invisible data loss, which is the hardest class of
// bug to notice here.
func EventsFromChangelogEntry(ic IssueContext, entry map[string]any) ([]events.Event, error) {
	id, ok := entry["id"].(string)
	if !ok || id == "" {
		return nil, fmt.Errorf("changelog entry for %s has no usable id field", ic.Key)
	}

	occurredAt, err := parseJiraTime(entry["created"])
	if err != nil {
		return nil, fmt.Errorf("changelog entry %s on %s: created: %w", id, ic.Key, err)
	}

	accountID, displayName := author(entry["author"])

	items, ok := entry["items"].([]any)
	if !ok && entry["items"] != nil {
		return nil, fmt.Errorf("changelog entry %s on %s: items is %T, want a list", id, ic.Key, entry["items"])
	}

	var out []events.Event

	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("changelog entry %s on %s: item is %T, want an object", id, ic.Key, raw)
		}

		field, _ := item["field"].(string)

		segment, tracked := trackedFields[field]
		if !tracked {
			continue
		}

		from, _ := item["fromString"].(string)
		to, _ := item["toString"].(string)

		summary := fmt.Sprintf("%s %s: %s", ic.Key, field, to)
		if field == fieldStatus {
			summary = fmt.Sprintf("%s status: %s → %s", ic.Key, from, to)
		}

		// The field is part of the ExternalID, not just the entry id: two
		// tracked changes in one entry share the entry id, and would dedupe
		// against each other without it.
		evt := events.NewEvent(Name, fmt.Sprintf("%s:%s:%s", ic.Key, segment, id), occurredAt, summary)
		evt.Actor = displayName
		evt.RawRef = ic.browseURL()
		evt.Artifacts["field"] = field
		ic.annotate(&evt, accountID)

		out = append(out, evt)
	}

	return out, nil
}

// EventFromComment converts one comment into an event.
//
// Comments do not appear in an issue's changelog, so this is the only evidence
// that an issue has already been narrated — by unjira or by a human. Without it
// the reconciler proposes the same comment every pass.
func EventFromComment(ic IssueContext, comment map[string]any) (events.Event, error) {
	id, ok := comment["id"].(string)
	if !ok || id == "" {
		return events.Event{}, fmt.Errorf("comment on %s has no usable id field", ic.Key)
	}

	occurredAt, err := parseJiraTime(comment["created"])
	if err != nil {
		return events.Event{}, fmt.Errorf("comment %s on %s: created: %w", id, ic.Key, err)
	}

	accountID, displayName := author(comment["author"])
	body, _ := comment["body"].(string)

	// Full text, no cap. The worst case Jira permits in a comment is a small
	// fraction of one correlator prompt, and Cluster already bisects on
	// overflow — that machinery exists so events need not self-censor.
	evt := events.NewEvent(
		Name,
		fmt.Sprintf("%s:comment:%s", ic.Key, id),
		occurredAt,
		fmt.Sprintf("%s comment by %s: %s", ic.Key, displayName, body),
	)
	evt.Actor = displayName
	evt.RawRef = ic.browseURL()
	ic.annotate(&evt, accountID)

	return evt, nil
}

// annotate sets the artifacts common to every event this collector emits. It
// takes evt by pointer rather than mutating a by-value copy's Artifacts map:
// the latter happens to work (the map header is copied but points at the same
// backing storage) but is subtle enough that a future edit adding a
// non-map field assignment here would silently stop affecting the caller. A
// pointer receiver removes that trap.
func (ic IssueContext) annotate(evt *events.Event, authorAccountID string) {
	evt.Artifacts["issue_key"] = ic.Key
	evt.Artifacts["project_key"] = ic.ProjectKey
	evt.Artifacts["connection"] = ic.Connection
	evt.Artifacts["authored_by_unjira"] = ic.SelfAccountID != "" && authorAccountID == ic.SelfAccountID
}

// author pulls the accountId and display name out of a Jira author object.
// Both are optional in practice (deleted users, app-authored changes), so a
// missing author yields empty strings rather than an error.
func author(raw any) (accountID, displayName string) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return "", ""
	}

	accountID, _ = obj["accountId"].(string)
	displayName, _ = obj["displayName"].(string)

	return accountID, displayName
}

// parseJiraTime parses Jira's timestamp format.
func parseJiraTime(raw any) (time.Time, error) {
	s, ok := raw.(string)
	if !ok || s == "" {
		return time.Time{}, fmt.Errorf("missing or non-string value %v", raw)
	}

	t, err := time.Parse(jiraTimeFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing %q as %q: %w", s, jiraTimeFormat, err)
	}

	return t, nil
}
