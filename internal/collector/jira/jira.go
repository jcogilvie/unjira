package jira

import (
	"errors"
	"fmt"
	"log"
	"time"

	jiraclient "github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
)

// searchFields are the issue fields the search must return.
//
// summary and description are deliberately absent: they arrive as changelog
// events when they change, and requesting them here would invite emitting a
// snapshot on every pass — an event stream of "the description is still X",
// which is not an observation that anything happened.
var searchFields = []string{"key", "project", "updated"}

// Collector reads Jira changes for every configured connection's named queries.
type Collector struct{}

// New builds the collector. Stateless: everything it needs arrives on the
// CollectContext, which is what lets one instance serve every connection.
func New() *Collector { return &Collector{} }

// Name identifies this collector in the registry, the cursors table, and the
// Source field of every event it emits.
func (c *Collector) Name() string { return Name }

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

	// One Myself() call per connection per pass, not per issue. An empty
	// accountID (a permission that does not allow reading self) degrades to
	// "tag nothing", which is preferable to failing the whole pass — the tag is
	// an optimisation for the reconciler, not a correctness requirement of
	// collection.
	selfAccountID := ""
	if me, meErr := client.Myself(); meErr != nil {
		log.Printf("jira: connection %s: could not read own account id (%v); "+
			"self-authored changes will not be tagged this pass", conn.Name, meErr)
	} else {
		selfAccountID, _ = me["accountId"].(string)
	}

	var failures []error

	for _, query := range conn.Queries {
		if err := c.collectQuery(cc, conn, query, client, selfAccountID, limit, visit); err != nil {
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
		// Jira's JQL date literal has minute precision, so this floors to the
		// minute rather than dropping precision it cannot express. Flooring
		// re-examines a little (free, thanks to dedup); rounding up would skip.
		searchJQL = fmt.Sprintf("%s AND updated >= %q", effectiveJQL,
			watermark.UTC().Truncate(time.Minute).Format("2006-01-02 15:04"))
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

	if len(issues) >= limit {
		// Logged, never silent: a silent cap presents as a clean pass while
		// ignoring work. Because the watermark only advances on a complete
		// query, the next pass re-runs this range rather than skipping ahead.
		log.Printf("jira: query %s/%s hit its %d-issue limit; more issues may be unexamined this pass",
			conn.Name, query.Name, limit)
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

	return cc.Store.SetCursor(Name, resource, EncodePosition(effectiveJQL, highest))
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
