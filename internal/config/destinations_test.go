package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
)

// destinationsYAML: Jira work tracked in "work" (writable, the default), upstream work
// in "upstream" (read-only, mirroring nowhere), and partner work in "partner" (read-only,
// mirroring into "work").
const destinationsYAML = `
connections:
  - {name: j, kind: jira, endpoint: "https://org.atlassian.net"}
  - {name: gh, kind: github, endpoint: "https://api.github.com"}
  - {name: ghe, kind: github, endpoint: "https://ghe.example.com/api/v3"}
trackers:
  - {name: work, connection: j, scopes: ["DEVSBX"], writable_scopes: ["DEVSBX"], default_scope: "DEVSBX"}
  - {name: ops, connection: j, scopes: ["OPS"], writable_scopes: ["OPS"], default_scope: "OPS"}
  - {name: upstream, connection: gh, scopes: ["crossplane/*"]}
  - {name: partner, connection: gh, scopes: ["partner/*"], mirror_to: ["work", "ops"]}
  - {name: internal, connection: ghe, scopes: ["corp/*"]}
default_ticket_in: ["work"]
`

func destinationsConfig(t *testing.T) config.Config {
	t.Helper()

	cfg, err := loadYAML(t, destinationsYAML)
	require.NoError(t, err)

	return cfg
}

// TestUntrackedDestinations_NoKnownScopeGoesToDefaultTicketIn: work outside every
// tracker's scope — a contributor fork, a repository nobody tracks — is ticketed where
// default_ticket_in says.
func TestUntrackedDestinations_NoKnownScopeGoesToDefaultTicketIn(t *testing.T) {
	plan := destinationsConfig(t).UntrackedDestinations([]string{"github.com/me/crossplane"})

	assert.Equal(t, []config.Destination{{Tracker: "work", Scope: "DEVSBX"}}, plan.Destinations)
	assert.Empty(t, plan.Location)
}

// TestUntrackedDestinations_UpstreamWorkMirrorsNowhereByDefault is the motivating case:
// untracked work in crossplane's scope gets no Jira ticket, because crossplane's tracker
// is read-only and its mirror_to is empty. Recorded with a reason.
func TestUntrackedDestinations_UpstreamWorkMirrorsNowhereByDefault(t *testing.T) {
	plan := destinationsConfig(t).UntrackedDestinations([]string{"github.com/crossplane/crossplane"})

	assert.Empty(t, plan.Destinations)
	assert.Equal(t, "upstream", plan.Location)
	assert.Contains(t, plan.Reason, `"upstream"`)
	assert.Contains(t, plan.Reason, "mirror_to")
}

func TestUntrackedDestinations_MirrorToNamesEveryDestination(t *testing.T) {
	plan := destinationsConfig(t).UntrackedDestinations([]string{"github.com/partner/sdk"})

	assert.Equal(t, []config.Destination{
		{Tracker: "work", Scope: "DEVSBX"},
		{Tracker: "ops", Scope: "OPS"},
	}, plan.Destinations)
	assert.Equal(t, "partner", plan.Location)
}

// TestUntrackedDestinations_ForkRemotesDropOutByScope: of a working copy's remotes only
// those inside a tracker's scope decide, so a fork beside upstream does not make the
// location ambiguous.
func TestUntrackedDestinations_ForkRemotesDropOutByScope(t *testing.T) {
	plan := destinationsConfig(t).UntrackedDestinations([]string{
		"github.com/crossplane/crossplane", "github.com/me/crossplane", "github.com/other/crossplane",
	})

	assert.Equal(t, "upstream", plan.Location)
	assert.Empty(t, plan.Ambiguous)
}

// TestUntrackedDestinations_TwoTrackerMatchIsAmbiguous: remotes routing to different
// trackers are reported, and treated as no known scope — never guessed.
func TestUntrackedDestinations_TwoTrackerMatchIsAmbiguous(t *testing.T) {
	plan := destinationsConfig(t).UntrackedDestinations([]string{
		"github.com/crossplane/crossplane", "github.com/partner/sdk",
	})

	assert.Equal(t, []string{"partner", "upstream"}, plan.Ambiguous)
	assert.Empty(t, plan.Location)
	assert.Equal(t, []config.Destination{{Tracker: "work", Scope: "DEVSBX"}}, plan.Destinations,
		"ambiguous is no known scope, so default_ticket_in")
}

// TestUntrackedDestinations_TheHostMustBeTheTrackersOwn: a remote matches a scope only on
// the host its tracker's connection serves, so corp/x on github.com is not the GHES
// tracker's corp/* and crossplane/x on gitlab is not upstream's.
func TestUntrackedDestinations_TheHostMustBeTheTrackersOwn(t *testing.T) {
	cfg := destinationsConfig(t)

	assert.Equal(t, "internal", cfg.UntrackedDestinations([]string{"ghe.example.com/corp/x"}).Location)
	assert.Empty(t, cfg.UntrackedDestinations([]string{"github.com/corp/x"}).Location)
	assert.Empty(t, cfg.UntrackedDestinations([]string{"gitlab.com/crossplane/x"}).Location,
		"a host no github connection serves matches no scope")
}

func TestUntrackedDestinations_EmptyDefaultTicketInProposesNothing(t *testing.T) {
	cfg := destinationsConfig(t)
	cfg.DefaultTicketIn = nil

	plan := cfg.UntrackedDestinations(nil)

	assert.Empty(t, plan.Destinations)
	assert.Contains(t, plan.Reason, "default_ticket_in")
}

// TestIssueAcceptsWrites: a link on a tracker with no writer is no destination at all,
// whereas a readable-but-unwritable Jira scope stays one, gated at apply as it always was.
func TestIssueAcceptsWrites(t *testing.T) {
	cfg := destinationsConfig(t)

	ok, reason := cfg.IssueAcceptsWrites("crossplane/crossplane#1")
	assert.False(t, ok)
	assert.Contains(t, reason, "read-only")

	ok, _ = cfg.IssueAcceptsWrites("DEVSBX-1")
	assert.True(t, ok)

	ok, _ = cfg.IssueAcceptsWrites("SUMO-1")
	assert.True(t, ok, "an untracked project is gated at apply, where the remedy is named")
}

func TestConnectionHost(t *testing.T) {
	tests := []struct {
		conn config.Connection
		want string
	}{
		{config.Connection{Kind: config.KindGitHub, Endpoint: "https://api.github.com"}, "github.com"},
		{config.Connection{Kind: config.KindGitHub, Endpoint: "https://ghe.example.com/api/v3"}, "ghe.example.com"},
		{config.Connection{Kind: config.KindJira, Endpoint: "https://org.atlassian.net"}, "org.atlassian.net"},
		{config.Connection{Kind: config.KindLocal}, ""},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.conn.Host(), tt.conn.Endpoint)
	}
}
