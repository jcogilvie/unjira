package jira

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	jiraclient "github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/logging"
	"github.com/jcogilvie/unjira/internal/pipeline"
)

// searchFields are the issue fields the search must return.
//
// Two roles, and conflating them cost F14 its entire effect. A field in trackedFields
// produces an event when it CHANGES, from the changelog. A field here is additionally
// available as its CURRENT VALUE on the search result — which is what
// EventFromIssueBody reads to record an issue's own summary and description.
//
// F14 shipped with only the first half: the constructor was wired, its tests were
// green, and this list said {key, project, updated}, so it received "" for both values
// on every issue and returned false every time. A full re-collect produced zero body
// events out of 99 issues. The list is the seam, so the tests that guard it assert
// this list rather than either side of it.
//
// The comment this replaced argued the fields should stay out because "requesting them
// here would invite emitting a snapshot on every pass — an event stream of 'the
// description is still X'". That risk is real and it is answered elsewhere: the body
// event's ExternalID is `<KEY>:body:<updated-unix>`, which changes only when Jira's own
// `updated` changes, so an unchanged body dedupes at insert. Withholding the fields did
// not prevent the snapshot problem, it prevented the feature.
//
// fieldStatus is tracked but not requested here, and the asymmetry is informational
// rather than a rule. Status transitions come from the changelog as from→to movements,
// and current status comes live from GetIssue during verification, so nothing reads a
// resting status value today — it would be collected and never consulted (compare F6).
// If a reader for it appears, add it; the note is here so the omission reads as a
// consequence rather than an oversight.
var searchFields = []string{"key", "project", "updated", fieldSummary, fieldDescription}

// Collector reads Jira changes for every configured connection's named queries.
type Collector struct{}

// New builds the collector. Stateless: everything it needs arrives on the
// CollectContext, which is what lets one instance serve every connection.
func New() *Collector { return &Collector{} }

// Name identifies this collector in the registry, the cursors table, and the
// Source field of every event it emits.
func (c *Collector) Name() string { return Name }

// SuppliesStatusHistory implements pipeline.StatusHistorySource: a status
// transition changelog entry becomes an events.SetStatusChange-tagged event
// (see EventsFromChangelogEntry), so this collector is a status-event
// source for the reconciler's staleness guard whenever it is enabled. See
// pipeline.StatusHistorySource's doc comment for why this is a marker
// interface rather than a name check.
func (c *Collector) SuppliesStatusHistory() {}

var _ pipeline.StatusHistorySource = (*Collector)(nil)

// Collect walks every configured connection's queries.
//
// Failure is per query, not per pass: a 403 on one JQL (a revoked project
// permission) must not stop an unrelated query from progressing. Failed queries
// are accumulated and returned together, and their watermarks stay put so the
// next run retries that range automatically.
func (c *Collector) Collect(cc pipeline.CollectContext, visit func(events.Event)) error {
	var failures []error

	for _, conn := range cc.Config.Jira {
		if len(conn.Queries) == 0 {
			// Write-only connection: it routes project keys but collects
			// nothing. Not an error.
			continue
		}

		if err := c.collectConnection(cc, conn, visit); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}

// collectConnection builds one client for a connection and runs its queries.
func (c *Collector) collectConnection(
	cc pipeline.CollectContext,
	conn config.JiraConnection,
	visit func(events.Event),
) error {
	cred, ok := cc.Credentials.For(conn.Name)
	if !ok {
		return fmt.Errorf(
			"jira connection %q has no credential: set it in UNJIRA_JIRA_CREDENTIALS", conn.Name)
	}

	client, err := jiraclient.New(conn.Site, cred.Email, cred.Token)
	if err != nil {
		return fmt.Errorf("building jira client for connection %q: %w", conn.Name, err)
	}

	limit, err := conn.IssueLimit()
	if err != nil {
		return err
	}

	// One Myself() call per connection per pass, not per issue. It yields two
	// things: our own accountId (for the self-authored tag) and the account's
	// configured timezone (which JQL date literals are interpreted in — see
	// watermarkClause).
	//
	// An empty accountID (a permission that does not allow reading self)
	// degrades to "tag nothing", which is preferable to failing the whole pass —
	// the tag is an optimisation for the reconciler, not a correctness
	// requirement of collection.
	var selfAccountID, accountZone string

	if me, meErr := client.Myself(); meErr != nil {
		logging.For(cc.Log, "jira").Warn("could not read own account id",
			"connection", conn.Name, "err", meErr,
			"consequence", "self-authored changes will not be tagged this pass")
	} else {
		selfAccountID, _ = me["accountId"].(string)
		accountZone, _ = me["timeZone"].(string)
	}

	var failures []error

	for _, query := range conn.Queries {
		if err := c.collectQuery(
			cc, conn, query, client, selfAccountID, accountZone, limit, visit,
		); err != nil {
			failures = append(failures, fmt.Errorf("query %s/%s: %w", conn.Name, query.Name, err))
		}
	}

	return errors.Join(failures...)
}

// collectQuery runs one named query: search, then per-issue changelog and
// comments, then advance the watermark.
func (c *Collector) collectQuery(
	cc pipeline.CollectContext,
	conn config.JiraConnection,
	query config.JiraQuery,
	client *jiraclient.Client,
	selfAccountID string,
	accountZone string,
	limit int,
	visit func(events.Event),
) error {
	effectiveJQL, err := conn.EffectiveJQL(query)
	if err != nil {
		return err
	}

	resource := CursorResource(conn.Name, query.Name)

	position, err := cc.Store.GetCursor(Name, resource)
	if err != nil {
		return err
	}

	searchJQL := effectiveJQL
	if watermark, ok := DecodePosition(position, effectiveJQL); ok {
		searchJQL = effectiveJQL + " AND " + watermarkClause(watermark, accountZone, conn.Name, cc.Log)
	}

	// The issues are collected before fetching changelogs rather than emitted
	// from inside the visit callback, so a mid-search failure cannot leave the
	// search half-consumed while errors propagate.
	var (
		issues  []map[string]any
		highest time.Time
	)

	if err := client.SearchIssues(searchJQL, searchFields, limit, func(issue map[string]any) {
		issues = append(issues, issue)
	}); err != nil {
		return err
	}

	// A capped pass saw an ARBITRARY subset, so it must not record progress.
	//
	// The JQL carries no ORDER BY, so which `limit` issues come back is Jira's
	// choice. Advancing the watermark to the newest issue this pass happened to see
	// permanently excludes every OLDER issue it missed, because the next pass is
	// bounded by `updated >= <watermark>`. Measured on real data: a 50-issue cap
	// against ~114 matching issues stored a watermark newer than most of them, and
	// resetting the cursor by hand recovered 56 orphaned issues — 43 issues
	// collected before, 99 after.
	//
	// The asymmetry decides it. NOT advancing costs a re-fetch of events that
	// dedupe on (source, external_id) and are therefore free. Advancing costs work
	// that is never collected at all, and nothing surfaces the loss: the pass logs
	// a warning and then reports success.
	//
	// This block previously claimed "the watermark only advances on a complete
	// query", which was never true of the code below it, and which nothing tested —
	// the pre-existing cap test asserts only that a log line appears. See
	// truncation_test.go.
	capped := len(issues) >= limit
	if capped {
		logging.For(cc.Log, "jira").Warn("query hit its issue limit",
			"connection", conn.Name, "query", query.Name, "limit", limit,
			"consequence", "the remainder are unexamined, so this pass will NOT advance its "+
				"watermark and the next pass re-runs the same range",
			"action", "raise max_issues_per_query to make progress")
	}

	for _, issue := range issues {
		updated, err := c.collectIssue(conn, issue, client, selfAccountID, visit)
		if err != nil {
			// One issue's failure fails its query. Advancing past an issue we
			// could not read would step over it permanently.
			return err
		}

		if updated.After(highest) {
			highest = updated
		}
	}

	if highest.IsZero() {
		// Nothing matched; leave the existing watermark alone rather than
		// writing a zero one.
		return nil
	}

	if capped {
		// Every event this pass DID read is already persisted — the visit callback
		// ran, and events dedupe on (source, external_id). Only the watermark is
		// withheld, so the next pass re-examines the same range and can reach the
		// issues this one never saw.
		return nil
	}

	return cc.Store.SetCursor(Name, resource, EncodePosition(effectiveJQL, highest))
}

// jqlDateFormat is JQL's date-literal layout. Minute precision is all JQL
// expresses, so a watermark is floored (never rounded up) to it: flooring
// re-examines a little, which dedup makes free, while rounding up would skip.
const jqlDateFormat = "2006-01-02 15:04"

// watermarkZoneFallbackMargin is how far back the watermark is pushed when the
// account's timezone is unknown, covering the widest real UTC offset (UTC+14)
// plus an hour of slack.
//
// Erring backward is deliberate: too-early re-examines issues, which costs API
// calls and dedupes away, while too-late skips them permanently.
const watermarkZoneFallbackMargin = 15 * time.Hour

// watermarkClause renders the `updated >= "..."` bound for an incremental pass.
//
// **JQL date literals are interpreted in the account's configured timezone, not
// UTC.** This is the whole reason the function exists, and getting it wrong is
// invisible without a live test: measured against a real instance, an account in
// America/Indiana/Indianapolis (UTC-7) querying an issue updated at
// 12:02 local / 19:02 UTC matches on `updated >= "... 12:02"` and does NOT match
// on `updated >= "... 19:02"`. Sending UTC therefore pushes the bound hours into
// the future and silently under-collects on every incremental pass.
//
// It is silent because Jira answers HTTP 200 with an empty result set for a
// date literal it cannot use — it reserves 400 for structural syntax errors like
// an unclosed paren. A malformed or mis-zoned bound raises nothing; the query
// just matches less than it should, forever, while the watermark keeps
// advancing past work never read.
//
// An unknown or unloadable zone falls back to UTC minus
// watermarkZoneFallbackMargin rather than to bare UTC, and says so, because an
// over-wide window is recoverable and a too-narrow one is not.
func watermarkClause(watermark time.Time, accountZone, connName string, log *slog.Logger) string {
	if accountZone == "" {
		logging.For(log, "jira").Warn("account timezone unknown; widening the watermark",
			"connection", connName, "margin", watermarkZoneFallbackMargin,
			"consequence", "JQL dates are account-local, so a UTC bound could otherwise skip issues")

		return fmt.Sprintf("updated >= %q",
			watermark.Add(-watermarkZoneFallbackMargin).UTC().Truncate(time.Minute).Format(jqlDateFormat))
	}

	loc, err := time.LoadLocation(accountZone)
	if err != nil {
		logging.For(log, "jira").Warn("could not load account timezone; widening the watermark instead",
			"connection", connName, "zone", accountZone, "err", err,
			"margin", watermarkZoneFallbackMargin)

		return fmt.Sprintf("updated >= %q",
			watermark.Add(-watermarkZoneFallbackMargin).UTC().Truncate(time.Minute).Format(jqlDateFormat))
	}

	return fmt.Sprintf("updated >= %q", watermark.In(loc).Truncate(time.Minute).Format(jqlDateFormat))
}

// collectIssue emits every event for one issue and returns its updated time,
// which feeds the query's watermark.
func (c *Collector) collectIssue(
	conn config.JiraConnection,
	issue map[string]any,
	client *jiraclient.Client,
	selfAccountID string,
	visit func(events.Event),
) (time.Time, error) {
	key, _ := issue["key"].(string)
	if key == "" {
		return time.Time{}, fmt.Errorf("search result has no key field: %v", issue)
	}

	fields, _ := issue["fields"].(map[string]any)
	projectKey := ""
	if project, ok := fields["project"].(map[string]any); ok {
		projectKey, _ = project["key"].(string)
	}

	updated, err := parseJiraTime(fields["updated"])
	if err != nil {
		return time.Time{}, fmt.Errorf("issue %s: updated: %w", key, err)
	}

	ic := IssueContext{
		Key:           key,
		ProjectKey:    projectKey,
		Connection:    conn.Name,
		Site:          conn.Site,
		SelfAccountID: selfAccountID,
	}

	// The issue's own body, emitted BEFORE the changelog and comments — finding F14.
	// Every other constructor here derives from something that HAPPENED TO the issue,
	// so a description written at creation and never edited was invisible: 70 of 99
	// collected issues had no description text at all. No extra request is needed;
	// `fields` already holds it because the client asks for `fields=*all`.
	//
	// Skipped rather than failed when the body is empty. A ticket with a one-line
	// summary and no description is ordinary, unlike a changelog entry with no id.
	if evt, ok := EventFromIssueBody(
		ic, stringOf(fields[fieldSummary]), fields[fieldDescription], updated,
	); ok {
		visit(evt)
	}

	changelog, err := client.GetChangelog(key)
	if err != nil {
		return time.Time{}, fmt.Errorf("changelog for %s: %w", key, err)
	}

	for _, entry := range changelog {
		evts, err := EventsFromChangelogEntry(ic, entry)
		if err != nil {
			return time.Time{}, err
		}

		for _, evt := range evts {
			visit(evt)
		}
	}

	comments, err := client.GetComments(key)
	if err != nil {
		return time.Time{}, fmt.Errorf("comments for %s: %w", key, err)
	}

	for _, comment := range comments {
		evt, err := EventFromComment(ic, comment)
		if err != nil {
			return time.Time{}, err
		}

		visit(evt)
	}

	return updated, nil
}
