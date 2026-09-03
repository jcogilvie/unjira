package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrLocalIssueNotFound is returned by the local-issue accessors when no
// row matches the given key — unlike GetCursor's silent-empty-string
// convenience, "not found" is meaningful here and must not be swallowed.
var ErrLocalIssueNotFound = errors.New("local issue not found")

// localIssuesSchema is the local tasktracker backend's own mimicked issue
// store — distinct from narratives/actions in schema (store.go), which are
// unjira's own clustering/proposal records, not a mimicked tracker's issue
// records. Applied by Open alongside schema; kept in its own const because
// it is the only DDL with exactly one production consumer,
// internal/clients/local (see the package's own doc comment).
const localIssuesSchema = `
CREATE TABLE IF NOT EXISTS local_issues (
    key             TEXT PRIMARY KEY,
    project         TEXT NOT NULL,
    summary         TEXT NOT NULL,
    description     TEXT,
    issue_type      TEXT NOT NULL,
    -- The tracker's own status NAME, not a normalized category, because
    -- tasktracker.TaskWriter.SetStatus targets a name: a category cannot
    -- distinguish "In Review" from "Blocked" (see
    -- docs/superpowers/specs/2026-09-01-named-status-transitions-design.md),
    -- and a local backend that could not represent the distinction would make
    -- offline tests disagree with every real one.
    status          TEXT NOT NULL DEFAULT 'To Do',
    labels          TEXT NOT NULL DEFAULT '[]',
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS local_issue_comments (
    id         INTEGER PRIMARY KEY,
    issue_key  TEXT NOT NULL REFERENCES local_issues (key),
    body       TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
`

// -- local issues (the local tasktracker backend's own mimicked store) -----

// LocalIssue is one row of local_issues.
type LocalIssue struct {
	Key         string
	Project     string
	Summary     string
	Description string
	IssueType   string
	// Status is the tracker's own status name (e.g. "In Progress"), matching
	// what SetStatus targets. Named, not a category, for the reason on the
	// local_issues.status column.
	Status string
	Labels []string
}

// InsertLocalIssue creates a local issue, assigning it the next sequential
// "PROJECT-N" key for project. Returns the assigned key.
func (s *Store) InsertLocalIssue(project, summary, issueType, description string, labels []string) (string, error) {
	var maxN sql.NullInt64
	if err := s.db.QueryRow(
		`SELECT MAX(CAST(substr(key, length(?) + 2) AS INTEGER)) FROM local_issues WHERE project = ?`,
		project, project,
	).Scan(&maxN); err != nil {
		return "", fmt.Errorf("finding next local issue number for project %s: %w", project, err)
	}

	key := fmt.Sprintf("%s-%d", project, maxN.Int64+1)

	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return "", fmt.Errorf("marshaling labels for %s: %w", key, err)
	}

	_, err = s.db.Exec(
		`INSERT INTO local_issues (key, project, summary, description, issue_type, labels)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		key, project, summary, nullable(description), issueType, string(labelsJSON),
	)
	if err != nil {
		return "", fmt.Errorf("inserting local issue %s: %w", key, err)
	}

	return key, nil
}

// GetLocalIssue returns the local issue with the given key, or
// ErrLocalIssueNotFound if none exists.
func (s *Store) GetLocalIssue(key string) (LocalIssue, error) {
	var (
		issue       LocalIssue
		description sql.NullString
		labelsJSON  string
	)

	err := s.db.QueryRow(
		`SELECT key, project, summary, description, issue_type, status, labels
		 FROM local_issues WHERE key = ?`,
		key,
	).Scan(&issue.Key, &issue.Project, &issue.Summary, &description, &issue.IssueType, &issue.Status, &labelsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return LocalIssue{}, fmt.Errorf("getting local issue %s: %w", key, ErrLocalIssueNotFound)
	}
	if err != nil {
		return LocalIssue{}, fmt.Errorf("getting local issue %s: %w", key, err)
	}

	issue.Description = description.String

	if err := json.Unmarshal([]byte(labelsJSON), &issue.Labels); err != nil {
		return LocalIssue{}, fmt.Errorf("unmarshaling labels for %s: %w", key, err)
	}

	return issue, nil
}

// SetLocalIssueStatus sets the status NAME for the local issue with the given
// key, or returns ErrLocalIssueNotFound if none exists.
//
// Unvalidated by design: the local backend has no workflow, so any name is
// legal. Validating against a fixed list would make offline tests disagree with
// a real backend, whose legal names come from a live per-issue read.
func (s *Store) SetLocalIssueStatus(key, status string) error {
	res, err := s.db.Exec(
		`UPDATE local_issues
		 SET status = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
		 WHERE key = ?`,
		status, key,
	)
	if err != nil {
		return fmt.Errorf("setting status for local issue %s: %w", key, err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected for local issue %s: %w", key, err)
	}
	if rows == 0 {
		return fmt.Errorf("setting status for local issue %s: %w", key, ErrLocalIssueNotFound)
	}

	return nil
}

// InsertLocalIssueComment adds a comment to the local issue with the given
// key, or returns ErrLocalIssueNotFound if none exists.
func (s *Store) InsertLocalIssueComment(issueKey, body string) error {
	if _, err := s.GetLocalIssue(issueKey); err != nil {
		return fmt.Errorf("adding comment to local issue %s: %w", issueKey, err)
	}

	if _, err := s.db.Exec(
		`INSERT INTO local_issue_comments (issue_key, body) VALUES (?, ?)`,
		issueKey, body,
	); err != nil {
		return fmt.Errorf("adding comment to local issue %s: %w", issueKey, err)
	}

	return nil
}

// LocalIssueComments returns every comment body for issueKey, oldest first.
func (s *Store) LocalIssueComments(issueKey string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT body FROM local_issue_comments WHERE issue_key = ? ORDER BY id`,
		issueKey,
	)
	if err != nil {
		return nil, fmt.Errorf("querying comments for local issue %s: %w", issueKey, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, fmt.Errorf("scanning comment row for %s: %w", issueKey, err)
		}
		out = append(out, body)
	}

	return out, rows.Err()
}

// SearchLocalIssues returns up to limit local issues whose summary contains
// query (case-insensitive substring match; empty query matches all),
// ordered by key.
func (s *Store) SearchLocalIssues(query string, limit int) ([]LocalIssue, error) {
	rows, err := s.db.Query(
		`SELECT key, project, summary, description, issue_type, status, labels
		 FROM local_issues WHERE summary LIKE '%' || ? || '%' COLLATE NOCASE
		 ORDER BY key LIMIT ?`,
		query, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("searching local issues for %q: %w", query, err)
	}
	defer func() { _ = rows.Close() }()

	var out []LocalIssue
	for rows.Next() {
		var (
			issue       LocalIssue
			description sql.NullString
			labelsJSON  string
		)
		if err := rows.Scan(
			&issue.Key, &issue.Project, &issue.Summary, &description,
			&issue.IssueType, &issue.Status, &labelsJSON,
		); err != nil {
			return nil, fmt.Errorf("scanning local issue row: %w", err)
		}

		issue.Description = description.String
		if err := json.Unmarshal([]byte(labelsJSON), &issue.Labels); err != nil {
			return nil, fmt.Errorf("unmarshaling labels for %s: %w", issue.Key, err)
		}

		out = append(out, issue)
	}

	return out, rows.Err()
}
