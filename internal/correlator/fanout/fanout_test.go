package fanout_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/correlator/fanout"
)

func item(number int, title string) fanout.Item {
	return itemWith(number, title, "infra-repo", "alice")
}

func itemWith(number int, title, repo, author string) fanout.Item {
	return fanout.Item{Repo: repo, Author: author, Title: title, Number: number}
}

// -- NormalizeTitle -----------------------------------------------------

func TestNormalizeTitle_TrailingParenRegionFolds(t *testing.T) {
	a := fanout.NormalizeTitle("Switch shared-infra to managed mode (zrh)")
	b := fanout.NormalizeTitle("Switch shared-infra to managed mode (us2)")

	assert.Equal(t, a, b)
}

func TestNormalizeTitle_ForConnectorRegionFolds(t *testing.T) {
	assert.Equal(t,
		fanout.NormalizeTitle("Enable managed mode for zrh"),
		fanout.NormalizeTitle("Enable managed mode for us2"),
	)
}

func TestNormalizeTitle_RegionSubstringInRealWordIsUntouched(t *testing.T) {
	// "production" contains "prod" but must not fold — word-boundary gated.
	assert.Contains(t, fanout.NormalizeTitle("Ship the production release"), "production")
}

// -- ClusterFanout --------------------------------------------------------

func TestClusterFanout_TwelveRegionMirrorsCollapseToOneCluster(t *testing.T) {
	regions := []string{"zrh", "us2", "tky", "syd", "mon", "kor", "fra", "fed", "dub", "corp", "long", "prod"}

	var items []fanout.Item
	for i, region := range regions {
		number := 678 + i
		items = append(items, item(number, fmt.Sprintf("Switch shared-infra to managed mode (%s)", region)))
	}

	clusters := fanout.ClusterFanout(items)

	assert.Len(t, clusters, 1)
	c := clusters[0]
	assert.True(t, c.IsFanout())
	assert.Equal(t, [2]int{678, 689}, c.Span())

	wantNumbers := make([]int, 12)
	for i := range wantNumbers {
		wantNumbers[i] = 678 + i
	}
	assert.Equal(t, wantNumbers, c.Numbers)
}

func TestClusterFanout_NonConsecutiveNumbersSplitByGap(t *testing.T) {
	items := []fanout.Item{
		item(678, "Bump base image for zrh"),
		item(680, "Bump base image for us2"),
	}

	clusters := fanout.ClusterFanout(items) // default MaxGap=1; gap of 2 splits

	assert.Len(t, clusters, 2)
}

func TestClusterFanout_DifferentRepoDoesNotMerge(t *testing.T) {
	items := []fanout.Item{
		itemWith(1, "Enable managed mode for zrh", "repo-a", "alice"),
		itemWith(2, "Enable managed mode for us2", "repo-b", "alice"),
	}

	assert.Len(t, fanout.ClusterFanout(items), 2)
}

func TestClusterFanout_LoneItemIsSizeOneNonFanout(t *testing.T) {
	clusters := fanout.ClusterFanout([]fanout.Item{item(42, "One-off fix for the widget")})

	assert.Len(t, clusters, 1)
	assert.False(t, clusters[0].IsFanout())
	assert.Equal(t, []int{42}, clusters[0].Numbers)
}
