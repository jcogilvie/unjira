// Package fanout implements env-mirror fan-out clustering (see
// docs/design-notes.md #3).
//
// Infra work fans out: one logical change ("switch the shared module to
// managed mode") becomes ~12 near-identical PRs, one per region
// (repo#678-689). Reviewing all 12 is one effort, not 12 — and counting them
// as 12 rows inflates velocity and buries the single decision.
//
// NormalizeTitle folds region tokens so mirror titles compare equal;
// ClusterFanout groups by (repo, author, normalized title) and then splits
// each group into runs of consecutive PR numbers. Same author + same
// normalized title + adjacent numbers => one narrative with a range.
//
// Deliberately deterministic and evidence-only: it decides nothing about
// meaning (that is the LLM correlator's job) — it just collapses the
// mechanical fan-out so the correlator sees one story instead of twelve
// fragments.
package fanout

import (
	"regexp"
	"sort"
	"strings"
)

// regions are the known deployment regions/environments that mirror PRs fan
// out across.
var regions = []string{
	"zrh", "us2", "tky", "syd", "mon", "kor",
	"fra", "fed", "dub", "corp", "long", "prod", "dev", "stag",
}

var (
	regionAlt = strings.Join(regions, "|")

	// trailingParenRE matches a trailing "(region)" qualifier, e.g.
	// "... managed mode (zrh)".
	trailingParenRE = regexp.MustCompile(`(?i)\s*\((?:` + regionAlt + `)\)\s*$`)

	// connectorRegionRE matches a connector + region, e.g. "for zrh" /
	// "in us2" / "switch tky". Keeps the connector, drops the region.
	connectorRegionRE = regexp.MustCompile(`(?i)\b(for|in|switch)\s+(?:` + regionAlt + `)\b`)

	// suffixRegionRE matches a "-region" suffix token, e.g.
	// "bump-base-zrh". Word-boundary gated so "-production" is safe.
	suffixRegionRE = regexp.MustCompile(`(?i)-(?:` + regionAlt + `)\b`)

	whitespaceRE = regexp.MustCompile(`\s+`)
)

// NormalizeTitle folds region tokens out of title so env-mirror variants
// compare equal.
//
// Word-boundary gated: "production" (which contains "prod") is left intact.
// Case- and whitespace-insensitive; returns a lowercased, whitespace-collapsed
// key.
func NormalizeTitle(title string) string {
	text := trailingParenRE.ReplaceAllString(title, "")
	text = connectorRegionRE.ReplaceAllString(text, "$1")
	text = suffixRegionRE.ReplaceAllString(text, "")
	text = whitespaceRE.ReplaceAllString(text, " ")

	return strings.ToLower(strings.TrimSpace(text))
}

// Item is one reviewable unit (a PR) considered for fan-out clustering.
type Item struct {
	Repo   string
	Author string
	Title  string
	Number int
}

// Cluster is a run of same-repo/author/normalized-title items with
// consecutive numbers.
type Cluster struct {
	Repo            string
	Author          string
	NormalizedTitle string
	Numbers         []int
}

// Span returns the [min, max] of Numbers.
func (c Cluster) Span() [2]int {
	return [2]int{c.Numbers[0], c.Numbers[len(c.Numbers)-1]}
}

// IsFanout reports whether this cluster has more than one member.
func (c Cluster) IsFanout() bool {
	return len(c.Numbers) > 1
}

// groupKey identifies items sharing (repo, author, normalized title).
type groupKey struct {
	repo, author, normalizedTitle string
}

// ClusterFanout collapses env-mirror fan-out into clusters.
//
// Items sharing (repo, author, normalized title) are grouped; each group is
// split into runs where consecutive numbers differ by at most maxGap.
// Singletons come back as size-1, non-fanout clusters. Groups are returned
// in first-seen order; runs within a group ascend.
func ClusterFanout(items []Item, opts ...ClusterOption) []Cluster {
	o := clusterOptions{maxGap: 1}
	for _, opt := range opts {
		opt(&o)
	}

	var order []groupKey
	groups := make(map[groupKey][]Item)

	for _, it := range items {
		key := groupKey{it.Repo, it.Author, NormalizeTitle(it.Title)}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], it)
	}

	var clusters []Cluster
	for _, key := range order {
		numbers := uniqueSortedNumbers(groups[key])

		run := []int{numbers[0]}
		for _, n := range numbers[1:] {
			if n-run[len(run)-1] <= o.maxGap {
				run = append(run, n)
				continue
			}
			clusters = append(clusters, Cluster{key.repo, key.author, key.normalizedTitle, run})
			run = []int{n}
		}
		clusters = append(clusters, Cluster{key.repo, key.author, key.normalizedTitle, run})
	}

	return clusters
}

// clusterOptions holds the resolved settings for ClusterFanout.
type clusterOptions struct {
	maxGap int
}

// ClusterOption configures ClusterFanout.
type ClusterOption func(*clusterOptions)

// WithMaxGap overrides the maximum gap between consecutive numbers within a
// run (default 1).
func WithMaxGap(maxGap int) ClusterOption {
	return func(o *clusterOptions) { o.maxGap = maxGap }
}

func uniqueSortedNumbers(items []Item) []int {
	seen := make(map[int]bool, len(items))
	numbers := make([]int, 0, len(items))
	for _, it := range items {
		if !seen[it.Number] {
			seen[it.Number] = true
			numbers = append(numbers, it.Number)
		}
	}
	sort.Ints(numbers)

	return numbers
}
