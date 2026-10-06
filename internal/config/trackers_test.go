package config_test

// trackers_test.go covers the tracker model: connections (who and where), trackers
// (what scopes, and which of them are writable), and the top-level default_ticket_in.
// See docs/superpowers/specs/2026-10-06-tracker-model-design.md.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
)

// loadYAML writes body to a temporary unjira.config.yaml and loads it.
func loadYAML(t *testing.T, body string) (config.Config, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "unjira.config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return config.Load(path)
}

// validTrackerYAML is a minimal valid tracker model: one Jira tracker reading two
// projects and writing one, plus a read-only GitHub tracker.
const validTrackerYAML = `
connections:
  - name: work-jira
    kind: jira
    endpoint: https://org.atlassian.net
  - name: github
    kind: github
    endpoint: https://api.github.com
trackers:
  - name: work
    connection: work-jira
    scopes: ["PAAS", "DEVSBX"]
    writable_scopes: ["DEVSBX"]
    default_scope: "DEVSBX"
    queries:
      - name: mine
        jql: "assignee = currentUser()"
    max_issues_per_query: 50
  - name: upstream
    connection: github
    scopes: ["crossplane/crossplane", "crossplane-contrib/*"]
default_ticket_in: ["work"]
`

func TestLoad_ParsesTrackerModel(t *testing.T) {
	cfg, err := loadYAML(t, validTrackerYAML)
	require.NoError(t, err)

	require.Len(t, cfg.Connections, 2)
	assert.Equal(t, config.Connection{Name: "work-jira", Kind: config.KindJira, Endpoint: "https://org.atlassian.net"},
		cfg.Connections[0])

	work, ok := cfg.TrackerByName("work")
	require.True(t, ok)
	assert.Equal(t, "work-jira", work.Connection)
	assert.Equal(t, []string{"PAAS", "DEVSBX"}, work.Scopes)
	assert.Equal(t, []string{"DEVSBX"}, work.WritableScopes)
	assert.Equal(t, "DEVSBX", work.DefaultScope)
	assert.Equal(t, []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}}, work.Queries)
	assert.Equal(t, 50, work.MaxIssuesPerQuery)
	assert.Equal(t, []string{"work"}, cfg.DefaultTicketIn)
}

// TestLoad_ExampleConfigIsValid loads the shipped example itself, so the template a
// newcomer copies can never drift into something Load refuses.
func TestLoad_ExampleConfigIsValid(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "config", "unjira.example.yaml"))
	require.NoError(t, err)

	assert.NotEmpty(t, cfg.Connections)
	assert.NotEmpty(t, cfg.Trackers)
	for _, tr := range cfg.Trackers {
		assert.Empty(t, tr.WritableScopes,
			"tracker %q: a fresh clone must write nothing, so the example arms no scope", tr.Name)
	}
}

func TestLoad_JSONIsStillAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unjira.config.json")
	body := `{"connections": [{"name": "local", "kind": "local"}],
	  "trackers": [{"name": "dev", "connection": "local", "scopes": ["PROJ"]}]}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	require.Len(t, cfg.Trackers, 1)
	assert.Equal(t, []string{"PROJ"}, cfg.Trackers[0].Scopes)
}

func TestLoad_UnknownKeyIsAnError(t *testing.T) {
	_, err := loadYAML(t, "db_path: data/unjira.db\ndb_pth: typo\n")

	require.ErrorContains(t, err, "db_pth", "strict mode: a typo must fail at load, not be ignored")
}

// TestLoad_RefusesRemovedKeys: the old jira and tracker keys are refused with an error
// naming the new shape, never silently mapped (the store has no migrations, and config
// is user-local, so a silent mapping would be a second source of truth).
func TestLoad_RefusesRemovedKeys(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantText []string
	}{
		{
			name:     "jira",
			body:     `{"jira": [{"name": "default", "site": "https://x.atlassian.net", "project_keys": ["PROJ"]}]}`,
			wantText: []string{`"jira"`, "connections", "trackers", "scopes", "writable_scopes"},
		},
		{
			name:     "tracker",
			body:     `{"tracker": {"backend": "jira", "default_project": "PROJ"}}`,
			wantText: []string{`"tracker"`, "kind", "default_ticket_in", "default_scope"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadYAML(t, tt.body)

			require.Error(t, err)
			for _, want := range tt.wantText {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestLoad_RefusesBothRemovedKeysAtOnce(t *testing.T) {
	_, err := loadYAML(t, `{"jira": [], "tracker": {}}`)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"jira"`)
	assert.Contains(t, err.Error(), `"tracker"`, "report every removed key, not just the first")
}

// TestLoad_UnquotedYAMLBooleanScopeIsRefused: YAML 1.1 reads an unquoted NO, ON or YES
// as a boolean, and sigs.k8s.io/yaml then renders it into a string field as "false" or
// "true" WITHOUT an error. So the load must fail on the resulting scope, and say why.
func TestLoad_UnquotedYAMLBooleanScopeIsRefused(t *testing.T) {
	for _, word := range []string{"NO", "ON", "YES", "OFF"} {
		t.Run(word, func(t *testing.T) {
			_, err := loadYAML(t, `
connections: [{name: j, kind: jira, endpoint: "https://x.atlassian.net"}]
trackers:
  - name: work
    connection: j
    scopes: [PAAS, `+word+`]
`)

			require.Error(t, err, "%s must not load as a scope named true/false", word)
			// "quote the value", not "quote": the temp path carries this test's name, "Unquoted".
			assert.Contains(t, err.Error(), "quote the value")
			assert.Contains(t, err.Error(), "trackers[0].scopes")
		})
	}
}

func TestLoad_QuotedShortScopeLoads(t *testing.T) {
	cfg, err := loadYAML(t, `
connections: [{name: j, kind: jira, endpoint: "https://x.atlassian.net"}]
trackers: [{name: work, connection: j, scopes: ["NO", "ON"]}]
`)

	require.NoError(t, err)
	assert.Equal(t, []string{"NO", "ON"}, cfg.Trackers[0].Scopes)
}

func TestValidateTrackers_RefusesOverlappingScopes(t *testing.T) {
	tests := []struct {
		name   string
		first  string
		second string
		conn   string
	}{
		{name: "same project key", first: `"PAAS"`, second: `"PAAS"`, conn: "j"},
		{name: "same repo, different case", first: `"crossplane/crossplane"`, second: `"Crossplane/Crossplane"`, conn: "gh"},
		{name: "glob covers a literal repo", first: `"crossplane-contrib/*"`, second: `"crossplane-contrib/provider-aws"`, conn: "gh"},
		{name: "two equal globs", first: `"crossplane-contrib/*"`, second: `"CROSSPLANE-contrib/*"`, conn: "gh"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadYAML(t, `
connections:
  - {name: j, kind: jira, endpoint: "https://x.atlassian.net"}
  - {name: gh, kind: github, endpoint: "https://api.github.com"}
trackers:
  - {name: one, connection: `+tt.conn+`, scopes: [`+tt.first+`]}
  - {name: two, connection: `+tt.conn+`, scopes: [`+tt.second+`]}
`)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "overlap")
			assert.Contains(t, err.Error(), `"one"`)
			assert.Contains(t, err.Error(), `"two"`)
		})
	}
}

// TestValidateTrackers_ProjectScopesOverlapAcrossJiraAndLocal: a local tracker mints
// keys in the same <PROJECT>-<N> syntax as Jira, so the two kinds share a key space and
// one project cannot route to both.
func TestValidateTrackers_ProjectScopesOverlapAcrossJiraAndLocal(t *testing.T) {
	_, err := loadYAML(t, `
connections:
  - {name: j, kind: jira, endpoint: "https://x.atlassian.net"}
  - {name: l, kind: local}
trackers:
  - {name: one, connection: j, scopes: ["PROJ"]}
  - {name: two, connection: l, scopes: ["PROJ"]}
`)

	require.ErrorContains(t, err, "overlap")
}

func TestValidateTrackers_DisjointScopesAreValid(t *testing.T) {
	_, err := loadYAML(t, `
connections:
  - {name: j, kind: jira, endpoint: "https://x.atlassian.net"}
  - {name: gh, kind: github, endpoint: "https://api.github.com"}
trackers:
  - {name: a, connection: j, scopes: ["PAAS"]}
  - {name: b, connection: j, scopes: ["SUMO"]}
  - {name: c, connection: gh, scopes: ["crossplane/crossplane", "crossplane-contrib/*"]}
  - {name: d, connection: gh, scopes: ["crossplane/crossplane-runtime"]}
`)

	require.NoError(t, err)
}

// TestValidateTrackers_RefusesWritableScopeOnWriterlessKind: unjira has no GitHub
// writer, so a writable_scopes entry on a github tracker would arm nothing. Refused
// rather than ignored, because an operator who wrote it believes writes are possible.
func TestValidateTrackers_RefusesWritableScopeOnWriterlessKind(t *testing.T) {
	_, err := loadYAML(t, `
connections: [{name: gh, kind: github, endpoint: "https://api.github.com"}]
trackers:
  - name: upstream
    connection: gh
    scopes: ["crossplane/crossplane"]
    writable_scopes: ["crossplane/crossplane"]
`)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "writable_scopes")
	assert.Contains(t, err.Error(), `"github"`)
	assert.Contains(t, err.Error(), "no writer")
}

func TestValidateTrackers(t *testing.T) {
	const conns = `
connections:
  - {name: j, kind: jira, endpoint: "https://x.atlassian.net"}
  - {name: gh, kind: github, endpoint: "https://api.github.com"}
  - {name: l, kind: local}
`

	tests := []struct {
		name    string
		body    string
		wantErr string // substring; "" means valid
	}{
		{
			name:    "unknown connection",
			body:    conns + `trackers: [{name: t, connection: nope, scopes: ["PAAS"]}]`,
			wantErr: `trackers[0].connection "nope"`,
		},
		{
			name:    "writable not a subset of scopes",
			body:    conns + `trackers: [{name: t, connection: j, scopes: ["PAAS"], writable_scopes: ["DEVSBX"]}]`,
			wantErr: `writable_scopes includes "DEVSBX", which is not in scopes`,
		},
		{
			name:    "default scope not writable",
			body:    conns + `trackers: [{name: t, connection: j, scopes: ["PAAS"], default_scope: "PAAS"}]`,
			wantErr: `default_scope "PAAS" is not in writable_scopes`,
		},
		{
			name:    "unknown mirror_to",
			body:    conns + `trackers: [{name: t, connection: j, scopes: ["PAAS"], mirror_to: ["ghost"]}]`,
			wantErr: `trackers[0].mirror_to names "ghost"`,
		},
		{
			name: "mirror_to a read-only tracker",
			body: conns + `trackers:
  - {name: t, connection: j, scopes: ["PAAS"], mirror_to: ["up"]}
  - {name: up, connection: gh, scopes: ["o/r"]}`,
			wantErr: `"up" is a destination`,
		},
		{
			name:    "mirror_to itself",
			body:    conns + `trackers: [{name: t, connection: j, scopes: ["PAAS"], writable_scopes: ["PAAS"], default_scope: "PAAS", mirror_to: ["t"]}]`,
			wantErr: "names itself",
		},
		{
			name:    "unknown default_ticket_in",
			body:    conns + `trackers: [{name: t, connection: j, scopes: ["PAAS"]}]` + "\ndefault_ticket_in: [ghost]",
			wantErr: `default_ticket_in names "ghost"`,
		},
		{
			name:    "default_ticket_in a writable tracker with no default_scope",
			body:    conns + `trackers: [{name: t, connection: j, scopes: ["PAAS"], writable_scopes: ["PAAS"]}]` + "\ndefault_ticket_in: [t]",
			wantErr: "default_scope is required",
		},
		{
			name:    "default_ticket_in a read-only tracker",
			body:    conns + `trackers: [{name: t, connection: j, scopes: ["PAAS"]}]` + "\ndefault_ticket_in: [t]",
			wantErr: `"t" is a destination`,
		},
		{
			name: "several default_ticket_in",
			body: conns + `trackers:
  - {name: a, connection: j, scopes: ["PAAS"], writable_scopes: ["PAAS"], default_scope: "PAAS"}
  - {name: b, connection: l, scopes: ["PROJ"], writable_scopes: ["PROJ"], default_scope: "PROJ"}
default_ticket_in: [a, b]`,
		},
		{
			name:    "endpoint that is not a URL",
			body:    `connections: [{name: x, kind: github, endpoint: "api.github.com"}]`,
			wantErr: `connections[0].endpoint "api.github.com" is not an absolute URL`,
		},
		{
			name: "writable default_ticket_in with default_scope",
			body: conns + `trackers: [{name: t, connection: j, scopes: ["PAAS"], writable_scopes: ["PAAS"], default_scope: "PAAS"}]` +
				"\ndefault_ticket_in: [t]",
		},
		{
			name:    "duplicate tracker name",
			body:    conns + `trackers: [{name: t, connection: j, scopes: ["PAAS"]}, {name: t, connection: j, scopes: ["SUMO"]}]`,
			wantErr: `duplicate tracker name "t"`,
		},
		{
			name:    "duplicate connection name",
			body:    `connections: [{name: j, kind: local}, {name: j, kind: local}]`,
			wantErr: `duplicate connection name "j"`,
		},
		{
			name:    "unknown kind",
			body:    `connections: [{name: x, kind: trello, endpoint: "https://trello.com"}]`,
			wantErr: `kind "trello"`,
		},
		{
			name:    "jira connection without an endpoint",
			body:    `connections: [{name: x, kind: jira}]`,
			wantErr: "connections[0].endpoint is required",
		},
		{
			name:    "local connection with an endpoint",
			body:    `connections: [{name: x, kind: local, endpoint: "https://x"}]`,
			wantErr: "connections[0].endpoint",
		},
		{
			name:    "tracker with no scopes",
			body:    conns + `trackers: [{name: t, connection: j}]`,
			wantErr: "trackers[0].scopes is empty",
		},
		{
			name:    "lowercase jira scope",
			body:    conns + `trackers: [{name: t, connection: j, scopes: ["paas"]}]`,
			wantErr: `trackers[0].scopes: "paas"`,
		},
		{
			name:    "github scope without a repo",
			body:    conns + `trackers: [{name: t, connection: gh, scopes: ["crossplane"]}]`,
			wantErr: `trackers[0].scopes: "crossplane"`,
		},
		{
			name:    "github scope with an owner glob",
			body:    conns + `trackers: [{name: t, connection: gh, scopes: ["*/crossplane"]}]`,
			wantErr: `trackers[0].scopes: "*/crossplane"`,
		},
		{
			name:    "queries on a non-jira tracker",
			body:    conns + `trackers: [{name: t, connection: gh, scopes: ["o/r"], queries: [{name: q, jql: "x"}]}]`,
			wantErr: "trackers[0].queries",
		},
		{
			name: "duplicate query name on one connection",
			body: conns + `trackers:
  - {name: a, connection: j, scopes: ["PAAS"], queries: [{name: mine, jql: "x"}]}
  - {name: b, connection: j, scopes: ["SUMO"], queries: [{name: mine, jql: "y"}]}`,
			wantErr: `query name "mine"`,
		},
		{
			name: "same query name on different connections",
			body: `connections:
  - {name: j1, kind: jira, endpoint: "https://a.atlassian.net"}
  - {name: j2, kind: jira, endpoint: "https://b.atlassian.net"}
trackers:
  - {name: a, connection: j1, scopes: ["PAAS"], queries: [{name: mine, jql: "x"}]}
  - {name: b, connection: j2, scopes: ["SUMO"], queries: [{name: mine, jql: "y"}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadYAML(t, tt.body)

			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestLoad_DiscoversTheDefaultConfigFile(t *testing.T) {
	for _, name := range []string{"unjira.config.yaml", "unjira.config.yml", "unjira.config.json"} {
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			require.NoError(t, os.WriteFile(name, []byte(`{"db_path": "found.db"}`), 0o600))

			cfg, err := config.Load("")

			require.NoError(t, err)
			assert.Equal(t, "found.db", cfg.DBPath)
		})
	}
}

// TestLoad_TwoDefaultConfigFilesAreAmbiguous: silently preferring one would leave an
// operator editing a file unjira never reads.
func TestLoad_TwoDefaultConfigFilesAreAmbiguous(t *testing.T) {
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile("unjira.config.yaml", []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile("unjira.config.json", []byte(`{}`), 0o600))

	_, err := config.Load("")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unjira.config.yaml")
	assert.Contains(t, err.Error(), "unjira.config.json")
}

func TestLoad_NoDefaultConfigFileReturnsDefaults(t *testing.T) {
	t.Chdir(t.TempDir())

	cfg, err := config.Load("")

	require.NoError(t, err)
	assert.Equal(t, config.Default(), cfg)
}

func TestTracker_EffectiveJQLScopesToScopes(t *testing.T) {
	tr := config.Tracker{Name: "work", Scopes: []string{"PROJ", "OPS"}}

	got, err := tr.EffectiveJQL(config.JiraQuery{Name: "mine", JQL: "assignee = currentUser()"})

	require.NoError(t, err)
	assert.Equal(t, `(assignee = currentUser()) AND project IN ("PROJ", "OPS")`, got,
		"collection must be bounded to the tracker's own scopes")
}

func TestTracker_EffectiveJQLErrorsWithoutScopes(t *testing.T) {
	tr := config.Tracker{Name: "work"}

	_, err := tr.EffectiveJQL(config.JiraQuery{Name: "mine", JQL: "assignee = currentUser()"})

	require.Error(t, err, "an unscoped query would collect issues no tracker routes")
	assert.Contains(t, err.Error(), `"work"`)
	assert.Contains(t, err.Error(), "scopes")
}

func TestTracker_IssueLimitDefaultsAndRejectsNegative(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		want       int
		wantErr    bool
	}{
		{name: "absent defaults", configured: 0, want: config.DefaultMaxIssuesPerQuery},
		{name: "explicit is honored", configured: 50, want: 50},
		{name: "negative is an error", configured: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.Tracker{Name: "work", MaxIssuesPerQuery: tt.configured}.IssueLimit()

			if tt.wantErr {
				require.ErrorContains(t, err, "max_issues_per_query")

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestTracker_IsScopeWritable_EmptyWritableSetDeniesEverything is the write-scope
// safety property: absent writable_scopes denies EVERY scope, including the ones the
// tracker reads. Deliberately not "inherit scopes when unset".
func TestTracker_IsScopeWritable_EmptyWritableSetDeniesEverything(t *testing.T) {
	tr := config.Tracker{Name: "dev", Scopes: []string{"PAAS", "DEVSBX"}}

	assert.False(t, tr.IsScopeWritable("PAAS"))
	assert.False(t, tr.IsScopeWritable("DEVSBX"))
}

func TestTracker_IsScopeWritable_OnlyListedScopesAreWritable(t *testing.T) {
	tr := config.Tracker{Name: "dev", Scopes: []string{"PAAS", "DEVSBX"}, WritableScopes: []string{"DEVSBX"}}

	assert.True(t, tr.IsScopeWritable("DEVSBX"))
	assert.False(t, tr.IsScopeWritable("PAAS"))
	assert.False(t, tr.IsScopeWritable("GHOST"))
}

func TestLoad_MissingWritableScopesDefaultsToNil(t *testing.T) {
	cfg, err := loadYAML(t, `
connections: [{name: j, kind: jira, endpoint: "https://x.atlassian.net"}]
trackers: [{name: dev, connection: j, scopes: ["PAAS"]}]
`)

	require.NoError(t, err)
	assert.Nil(t, cfg.Trackers[0].WritableScopes)
	assert.False(t, cfg.Trackers[0].IsScopeWritable("PAAS"))
}

func TestTrackerForScope(t *testing.T) {
	cfg, err := loadYAML(t, validTrackerYAML)
	require.NoError(t, err)

	work, ok := cfg.TrackerForScope("PAAS")
	require.True(t, ok)
	assert.Equal(t, "work", work.Name)

	_, ok = cfg.TrackerForScope("GHOST")
	assert.False(t, ok)
}

func TestConnectionOf(t *testing.T) {
	cfg, err := loadYAML(t, validTrackerYAML)
	require.NoError(t, err)

	work, _ := cfg.TrackerByName("work")
	conn, ok := cfg.ConnectionOf(work)

	require.True(t, ok)
	assert.Equal(t, config.KindJira, conn.Kind)
	assert.Equal(t, "https://org.atlassian.net", conn.Endpoint)
}

func TestDefaultCreateTarget(t *testing.T) {
	t.Run("unset is an error naming the key", func(t *testing.T) {
		_, err := config.Config{}.DefaultCreateTarget()

		require.ErrorContains(t, err, "default_ticket_in")
	})

	t.Run("resolves to the tracker and its default scope", func(t *testing.T) {
		cfg, err := loadYAML(t, validTrackerYAML)
		require.NoError(t, err)

		target, err := cfg.DefaultCreateTarget()

		require.NoError(t, err)
		assert.Equal(t, "work", target.Name)
		assert.Equal(t, "DEVSBX", target.DefaultScope)
		assert.Equal(t, "DEVSBX", cfg.DefaultCreateScope())
	})

	t.Run("no default scope reads as empty", func(t *testing.T) {
		assert.Empty(t, config.Config{}.DefaultCreateScope())
	})
}

// TestFirstProjectScope is the --project fallback dev tools use. It skips a tracker
// whose scopes are repositories, since a repo is not a project key.
func TestFirstProjectScope(t *testing.T) {
	cfg := config.Config{
		Connections: []config.Connection{
			{Name: "gh", Kind: config.KindGitHub, Endpoint: "https://api.github.com"},
			{Name: "j", Kind: config.KindJira, Endpoint: "https://x.atlassian.net"},
		},
		Trackers: []config.Tracker{
			{Name: "up", Connection: "gh", Scopes: []string{"o/r"}},
			{Name: "work", Connection: "j", Scopes: []string{"PAAS"}},
		},
	}

	got, ok := cfg.FirstProjectScope()

	require.True(t, ok)
	assert.Equal(t, "PAAS", got)

	_, ok = config.Config{}.FirstProjectScope()
	assert.False(t, ok)
}
