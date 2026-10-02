//go:build live

package live

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// evidenceBuilder builds an indexEvidence. Its defaults are "every probe ran and
// matched", so each case states only the probe it is about.
type evidenceBuilder struct{ ev indexEvidence }

func evidence() *evidenceBuilder {
	return &evidenceBuilder{ev: indexEvidence{
		scopedFresh:   matched,
		keyFresh:      matched,
		scopedIndexed: matched,
	}}
}

func (b *evidenceBuilder) jqlUnbuildable(err error) *evidenceBuilder {
	b.ev.jqlErr = err
	return b
}

func (b *evidenceBuilder) scopedFresh(o searchOutcome) *evidenceBuilder {
	b.ev.scopedFresh = o
	return b
}

func (b *evidenceBuilder) keyFresh(o searchOutcome) *evidenceBuilder {
	b.ev.keyFresh = o
	return b
}

func (b *evidenceBuilder) scopedIndexed(o searchOutcome) *evidenceBuilder {
	b.ev.scopedIndexed = o
	return b
}

func (b *evidenceBuilder) build() indexEvidence { return b.ev }

var (
	matched  = searchOutcome{matched: true}
	missed   = searchOutcome{}
	errored  = searchOutcome{err: errors.New("search unavailable")}
	errJQLed = errors.New("no project_keys")
)

// TestClassifyIndexMiss pins the decision table that decides whether a failed
// collector test blames the change under test or Jira's search index.
//
// The cases that matter most are the ones an earlier version got wrong or could
// not express:
//   - a JQL that does not match the issue even against its CURRENT state is a real
//     bug no matter what the index says. The old discriminator re-ran the
//     collector's own JQL against the index, so a broken EffectiveJQL looked exactly
//     like index lag and was reported as INFRASTRUCTURE.
//   - a check that could not run (an error, or a failed control) never yields a
//     confident verdict (design-notes #26).
func TestClassifyIndexMiss(t *testing.T) {
	tests := []struct {
		name string
		ev   indexEvidence
		want verdictClass
	}{
		{
			name: "query could not be built",
			ev:   evidence().jqlUnbuildable(errJQLed).build(),
			want: verdictUndetermined,
		},
		{
			name: "reconciled search of the collector's JQL failed",
			ev:   evidence().scopedFresh(errored).build(),
			want: verdictUndetermined,
		},
		{
			name: "collector's JQL excludes the issue's current state: a scoping bug, even with the index empty",
			ev:   evidence().scopedFresh(missed).scopedIndexed(missed).build(),
			want: verdictRealBug,
		},
		{
			name: "collector's JQL misses AND the bare-key control misses: reconcile itself is not working",
			ev:   evidence().scopedFresh(missed).keyFresh(missed).scopedIndexed(missed).build(),
			want: verdictUndetermined,
		},
		{
			name: "collector's JQL misses and the control errored",
			ev:   evidence().scopedFresh(missed).keyFresh(errored).build(),
			want: verdictUndetermined,
		},
		{
			name: "JQL matches fresh state and the index has it: the collector's bound or processing excluded it",
			ev:   evidence().build(),
			want: verdictRealBug,
		},
		{
			name: "JQL matches fresh state but the index does not have it: index lag",
			ev:   evidence().scopedIndexed(missed).build(),
			want: verdictInfrastructure,
		},
		{
			name: "JQL matches fresh state but the index search errored",
			ev:   evidence().scopedIndexed(errored).build(),
			want: verdictUndetermined,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyIndexMiss(tc.ev)

			assert.Equal(t, tc.want, got.class, "reason: %s", got.reason)
			assert.NotEmpty(t, got.reason, "every verdict must say why")
		})
	}
}

// TestClassifyFresh pins the pre-check run BEFORE any waiting: only a
// deterministic, consistent-read REAL BUG may stop the test early. Everything else
// lets the test proceed, so a broken instrument degrades the eventual verdict to
// UNDETERMINED instead of failing every run.
func TestClassifyFresh(t *testing.T) {
	tests := []struct {
		name       string
		ev         indexEvidence
		wantOK     bool
		wantClass  verdictClass
		wantReason bool
	}{
		{name: "JQL matches the current state", ev: evidence().build(), wantOK: true},
		{
			name: "scoping bug", ev: evidence().scopedFresh(missed).build(),
			wantClass: verdictRealBug, wantReason: true,
		},
		{
			name: "control failed", ev: evidence().scopedFresh(missed).keyFresh(missed).build(),
			wantClass: verdictUndetermined, wantReason: true,
		},
		{
			name: "reconciled search errored", ev: evidence().scopedFresh(errored).build(),
			wantClass: verdictUndetermined, wantReason: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classifyFresh(tc.ev)

			assert.Equal(t, tc.wantOK, ok)
			if !tc.wantOK {
				assert.Equal(t, tc.wantClass, got.class, "reason: %s", got.reason)
			}
			assert.Equal(t, tc.wantReason, got.reason != "")
		})
	}
}

// The annotation is the one line a PR author sees on the checks page without
// opening the log, so it must survive GitHub's workflow-command parser: a raw
// newline would end the command and drop the rest of the verdict.
func TestGitHubAnnotationEscapesWorkflowCommandData(t *testing.T) {
	got := githubAnnotation(verdict{class: verdictInfrastructure, reason: "100% lost\nre-run"}, "detail")

	assert.Equal(t,
		"::error title=Live Jira verdict%3A INFRASTRUCTURE::100%25 lost%0Are-run%0A%0Adetail",
		got)
}
