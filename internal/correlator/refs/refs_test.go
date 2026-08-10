package refs_test

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/correlator/refs"
)

func keys(t *testing.T, text string, opts ...refs.Option) []string {
	t.Helper()

	parsed, err := refs.ParsePRRefs(text, opts...)
	if err != nil {
		t.Fatalf("ParsePRRefs(%q) returned unexpected error: %v", text, err)
	}

	out := make([]string, len(parsed))
	for i, r := range parsed {
		out[i] = r.Key()
	}

	return out
}

func TestParsePRRefs_NoBareNumberCollision(t *testing.T) {
	ks := keys(t, "see repo-a#382 and org/repo-b#382")

	assert.Equal(t, []string{"repo-a#382", "org/repo-b#382"}, ks)
	assert.Len(t, uniq(ks), 2)
}

func TestParsePRRefs_HyphenRangeExpandsToMembers(t *testing.T) {
	ks := keys(t, "infra-repo#678-689")

	assert.Equal(t, rangeKeys("infra-repo", 678, 689), ks)
}

func TestParsePRRefs_EnDashRangeExpandsInclusive(t *testing.T) {
	ks := keys(t, "fan-out infra-repo#655–665 landed")

	want := rangeKeys("infra-repo", 655, 665)
	assert.Equal(t, want, ks)
	assert.Len(t, ks, 11)
}

func TestParsePRRefs_BareRefUsesDefaultRepoOrIsSkipped(t *testing.T) {
	assert.Equal(t, []string{"o/r#382"}, keys(t, "bumped #382", refs.WithDefaultRepo("o/r")))
	assert.Empty(t, keys(t, "bumped #382"))
}

func TestParsePRRefs_OversizedRangeErrorsLoudly(t *testing.T) {
	_, err := refs.ParsePRRefs("repo#1-100000", refs.WithMaxSpan(500))

	assert.Error(t, err)
}

func TestParsePRRefs_OrderPreservingAndDeduplicated(t *testing.T) {
	ks := keys(t, "repo#5 repo#5 repo#3-4 repo#4") //nolint:dupword // duplicate ref is the point of this test

	assert.Equal(t, []string{"repo#5", "repo#3", "repo#4"}, ks)
}

func TestParsePRRefs_InvertedRangeIsSingleRef(t *testing.T) {
	assert.Equal(t, []string{"repo#665"}, keys(t, "repo#665-655"))
}

func TestRef_IsComparableValueType(t *testing.T) {
	a := refs.Ref{Repo: "o/r", Number: 1}
	b := refs.Ref{Repo: "o/r", Number: 1}

	assert.Equal(t, a, b)
	assert.Equal(t, "o/r#1", a.Key())

	seen := map[refs.Ref]bool{a: true}
	seen[b] = true
	assert.Len(t, seen, 1)
}

func uniq(ss []string) map[string]bool {
	out := make(map[string]bool, len(ss))
	for _, s := range ss {
		out[s] = true
	}

	return out
}

func rangeKeys(repo string, start, end int) []string {
	out := make([]string, 0, end-start+1)
	for n := start; n <= end; n++ {
		out = append(out, repo+"#"+strconv.Itoa(n))
	}

	return out
}
