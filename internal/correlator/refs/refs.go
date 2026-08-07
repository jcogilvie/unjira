// Package refs parses fully-qualified, range-aware PR/issue references (see
// docs/design-notes.md #4).
//
// Two predecessor bugs motivate this package:
//
//   - Bare-number ambiguity. Once the scan went multi-org, #382 was ambiguous
//     (repo-a#382 != org/repo-b#382). A dedup key on the bare number silently
//     merged distinct PRs. Fix: key on the full owner/repo#N.
//   - Range collapse. Ledgers collapse fan-out families into ranges
//     (#655-665). A dedup set built from the literal text saw only the
//     endpoints and re-added the interior members as "new". Fix: expand
//     ranges into members before deduping.
//
// A reference is written owner/repo#N, repo#N, or bare #N; a bare ref only
// becomes a Ref when a default repo is supplied via WithDefaultRepo
// (otherwise it is too ambiguous to key on and is skipped). Ranges #A-B
// (hyphen or en dash) expand to inclusive members.
package refs

import (
	"fmt"
	"regexp"
	"strconv"
)

// DefaultMaxSpan is the maximum number of members a single range may expand
// to unless overridden with WithMaxSpan.
const DefaultMaxSpan = 500

// refRE matches an optional repo (directly abutting '#'), a start number, and
// an optional -end range. The repo group cannot span whitespace, so a bare
// "#382" (space before '#') leaves it empty.
var refRE = regexp.MustCompile(`([A-Za-z0-9._/-]+)?#(\d+)(?:[-\x{2013}](\d+))?`)

// Ref is a single fully-qualified PR/issue reference. Repo is as-written
// (may lack an owner).
type Ref struct {
	Repo   string
	Number int
}

// Key returns the fully-qualified dedup key, e.g. "owner/repo#123".
func (r Ref) Key() string {
	return fmt.Sprintf("%s#%d", r.Repo, r.Number)
}

// options holds the resolved settings for ParsePRRefs.
type options struct {
	defaultRepo string
	maxSpan     int
}

// Option configures ParsePRRefs.
type Option func(*options)

// WithDefaultRepo sets the repo attached to bare #N refs. Without it, bare
// refs are skipped as too ambiguous to key on.
func WithDefaultRepo(repo string) Option {
	return func(o *options) { o.defaultRepo = repo }
}

// WithMaxSpan overrides the maximum members a single range may expand to.
func WithMaxSpan(maxSpan int) Option {
	return func(o *options) { o.maxSpan = maxSpan }
}

// ParsePRRefs parses refs from text, expanding ranges to members,
// order-preserving and deduplicated.
//
// A range whose span exceeds the configured max span returns an error rather
// than silently truncating (docs/design-notes.md #9: error loudly).
func ParsePRRefs(text string, opts ...Option) ([]Ref, error) {
	o := options{maxSpan: DefaultMaxSpan}
	for _, opt := range opts {
		opt(&o)
	}

	seen := make(map[Ref]bool)
	var out []Ref

	for _, match := range refRE.FindAllStringSubmatch(text, -1) {
		repo := match[1]
		if repo == "" {
			repo = o.defaultRepo
		}

		if repo == "" {
			continue // bare ref with no default: too ambiguous to key on
		}

		start, err := strconv.Atoi(match[2])
		if err != nil {
			return nil, fmt.Errorf("parsing ref start number %q: %w", match[2], err)
		}

		numbers, err := expandRange(repo, start, match[3], o.maxSpan)
		if err != nil {
			return nil, err
		}

		for _, n := range numbers {
			ref := Ref{Repo: repo, Number: n}
			if !seen[ref] {
				seen[ref] = true
				out = append(out, ref)
			}
		}
	}

	return out, nil
}

// expandRange returns the single start number, or every member of the
// [start, end] range when endRaw is a valid, non-inverted range end.
func expandRange(repo string, start int, endRaw string, maxSpan int) ([]int, error) {
	if endRaw == "" {
		return []int{start}, nil
	}

	end, err := strconv.Atoi(endRaw)
	if err != nil {
		return nil, fmt.Errorf("parsing ref end number %q: %w", endRaw, err)
	}

	if end < start {
		// Inverted "range" — decline to interpret as a range.
		return []int{start}, nil
	}

	span := end - start + 1
	if span > maxSpan {
		return nil, fmt.Errorf(
			"range %s#%d-%d spans %d members (> max span %d); refusing to expand silently",
			repo, start, end, span, maxSpan,
		)
	}

	numbers := make([]int, 0, span)
	for n := start; n <= end; n++ {
		numbers = append(numbers, n)
	}

	return numbers, nil
}
