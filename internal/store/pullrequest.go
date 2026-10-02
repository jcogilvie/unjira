package store

import (
	"fmt"
	"strings"

	"github.com/jcogilvie/unjira/internal/events"
)

// PullRequestHolder is a narrative holding, as a MEMBER, at least one event that carries
// a given events.ArtifactPullRequest — and that narrative's status, which the caller
// needs to refuse a target that is no longer open.
type PullRequestHolder struct {
	NarrativeID int64
	Status      string
}

// pullRequestKeyChunk bounds one query's IN list, well under SQLite's bound-parameter
// limit, so a pass with an unusual number of PR events still issues valid SQL.
const pullRequestKeyChunk = 500

// PullRequestMemberHolders returns, for each of keys that any narrative holds, every
// narrative holding a member event whose events.ArtifactPullRequest equals it exactly,
// ascending by narrative id. A key nobody holds is absent from the map.
//
// The lookup behind the clustering pre-filter's pull-request identity join
// (internal/pipeline/preassign.go). It reports EVERY holder, rather than the one the
// caller hopes for, because more than one is a real state — the split F43 describes,
// already persisted — and the join must see it to refuse it. Member links only: a
// narrative holding a PR's event as background does not hold that PR's work.
//
// Not bounded by any window. A pull request merged days after it was opened is the
// steady state under `watch`, so the narrative holding its :opened event is usually
// outside the window being narrated; that is the case the join exists for.
func (s *Store) PullRequestMemberHolders(keys []string) (map[string][]PullRequestHolder, error) {
	return pullRequestMemberHolders(s.db, keys)
}

// PullRequestMemberHolders is the *Tx-scoped variant of (*Store).PullRequestMemberHolders,
// for a writer that must re-check the holders inside the transaction it writes in.
func (t *Tx) PullRequestMemberHolders(keys []string) (map[string][]PullRequestHolder, error) {
	return pullRequestMemberHolders(t.tx, keys)
}

func pullRequestMemberHolders(c dbConn, keys []string) (map[string][]PullRequestHolder, error) {
	out := make(map[string][]PullRequestHolder)

	for start := 0; start < len(keys); start += pullRequestKeyChunk {
		chunk := keys[start:min(start+pullRequestKeyChunk, len(keys))]
		if err := pullRequestMemberHoldersChunk(c, chunk, out); err != nil {
			return nil, err
		}
	}

	return out, nil
}

func pullRequestMemberHoldersChunk(c dbConn, keys []string, out map[string][]PullRequestHolder) error {
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}

	// json_extract rather than a LIKE over the artifacts blob, for the reason
	// LatestStatusEvent gives: a substring match would also match the value inside
	// another field. The artifact key is interpolated from events' declared constant so
	// this query and its writers cannot spell it differently.
	rows, err := c.Query(fmt.Sprintf(
		`SELECT DISTINCT json_extract(e.artifacts, '$.%[1]s'), n.id, n.status
		   FROM narrative_events ne
		   JOIN events e ON e.id = ne.event_id
		   JOIN narratives n ON n.id = ne.narrative_id
		  WHERE `+memberLink+`
		    AND json_extract(e.artifacts, '$.%[1]s') IN (%[2]s)
		  ORDER BY 1, n.id`,
		events.ArtifactPullRequest, strings.TrimSuffix(strings.Repeat("?, ", len(keys)), ", "),
	), args...)
	if err != nil {
		return fmt.Errorf("querying narratives holding %d pull request(s) as member work: %w", len(keys), err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			key string
			h   PullRequestHolder
		)
		if err := rows.Scan(&key, &h.NarrativeID, &h.Status); err != nil {
			return fmt.Errorf("scanning a pull request holder: %w", err)
		}
		out[key] = append(out[key], h)
	}

	return rows.Err()
}
