// Package devtools seeds and resets the dev Jira instance with realistic,
// labeled test data.
//
// Everything created here carries the unjira-seed label so Reset can find
// and remove exactly what Seed created, nothing else. Transitioning seeded
// issues along legal paths generates changelog history — the raw material
// for workflow mining and, later, estimation calibration. Doubles as the
// demo-environment builder.
package devtools

import (
	"fmt"
	"math/rand/v2"

	"github.com/jcogilvie/unjira/internal/clients/jira"
)

var seedSummaries = []string{
	"Increase resource requests for ingress controller",
	"Fix retry logic in payment client",
	"Add structured logging to auth service",
	"Investigate flaky checkout integration test",
	"Upgrade postgres client library",
	"Document rollback procedure for deploys",
	"Rate-limit the webhook receiver",
	"Migrate cron jobs to the new scheduler",
}

// options holds the resolved settings for Seed.
type options struct {
	rng *rand.Rand
}

// SeedOption configures Seed.
type SeedOption func(*options)

// WithSeed makes Seed's transition/comment randomness deterministic, for
// tests.
func WithSeed(seed uint64) SeedOption {
	return func(o *options) {
		o.rng = rand.New(rand.NewPCG(seed, seed))
	}
}

// Seed creates count labeled issues and walks each along 0-3 legal
// transitions, adding a comment about half the time. Returns the created
// keys.
func Seed(client *jira.Client, projectKey string, count int, opts ...SeedOption) ([]string, error) {
	o := options{rng: rand.New(rand.NewPCG(11, 11))}
	for _, opt := range opts {
		opt(&o)
	}

	keys := make([]string, 0, count)

	for i := 0; i < count; i++ {
		summary := seedSummaries[i%len(seedSummaries)]

		key, err := client.CreateIssue(
			projectKey,
			fmt.Sprintf("[seed] %s", summary),
			"Task",
			fmt.Sprintf("Seeded by unjira devtools (item %d of %d).", i+1, count),
			[]string{jira.SeedLabel},
		)
		if err != nil {
			return nil, fmt.Errorf("creating seed issue %d: %w", i+1, err)
		}
		keys = append(keys, key)

		transitionCount := o.rng.IntN(4) // 0-3 inclusive
		for j := 0; j < transitionCount; j++ {
			transitions, err := client.GetTransitions(key)
			if err != nil {
				return nil, fmt.Errorf("fetching transitions for %s: %w", key, err)
			}
			if len(transitions) == 0 {
				break
			}

			choice := transitions[o.rng.IntN(len(transitions))]
			transitionID, _ := choice["id"].(string)
			if err := client.TransitionIssue(key, transitionID, nil); err != nil {
				return nil, fmt.Errorf("transitioning %s: %w", key, err)
			}
		}

		if o.rng.Float64() < 0.5 {
			if _, err := client.AddComment(key, "Seeded comment: investigation notes go here."); err != nil {
				return nil, fmt.Errorf("commenting on %s: %w", key, err)
			}
		}
	}

	return keys, nil
}

// Reset deletes every seed-labeled issue in the project. Returns the
// deleted keys.
func Reset(client *jira.Client, projectKey string) ([]string, error) {
	jql := fmt.Sprintf(`project = "%s" AND labels = "%s"`, projectKey, jira.SeedLabel)

	var keys []string
	err := client.SearchIssues(jql, []string{"status"}, 500, func(issue map[string]any) {
		if key, ok := issue["key"].(string); ok {
			keys = append(keys, key)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("searching seed-labeled issues in %s: %w", projectKey, err)
	}

	for _, key := range keys {
		if err := client.DeleteIssue(key); err != nil {
			return nil, fmt.Errorf("deleting %s: %w", key, err)
		}
	}

	return keys, nil
}
