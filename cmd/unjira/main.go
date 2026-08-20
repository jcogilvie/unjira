// Package main implements the unjira CLI: collect | digest | status | dev.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/alecthomas/kong"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/clients/local"
	"github.com/jcogilvie/unjira/internal/clients/openai"
	"github.com/jcogilvie/unjira/internal/collector/claudecode"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/devtools"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
	"github.com/jcogilvie/unjira/internal/workflow"
)

const (
	// narrateLeaseTTL bounds how long a crashed narration pass can hold the
	// pipeline lock before another run may steal it. Generous relative to a
	// pass (minutes), since stealing a live lease is worse than waiting.
	narrateLeaseTTL = 30 * time.Minute
	// narrateLeasePoll is how often a blocked Acquire retries.
	narrateLeasePoll = 2 * time.Second
)

// registry maps collector names to factories, mirroring
// internal/collectors.REGISTRY in the Python implementation.
var registry = map[string]func() pipeline.Collector{
	"claude_code": func() pipeline.Collector { return claudecode.New() },
}

// appContext carries the loaded config, open store, and Jira credentials to
// every command.
type appContext struct {
	config          config.Config
	store           *store.Store
	jiraCredentials JiraCredentials
	llmAPIKey       string
}

// jiraCredential is one Jira connection's email/token pair.
type jiraCredential struct {
	Email string `json:"email"`
	Token string `json:"token"`
}

// JiraCredentials maps a config.JiraConnection.Name to its credential,
// decoded from a single JSON-object env var (UNJIRA_JIRA_CREDENTIALS), e.g.:
//
//	UNJIRA_JIRA_CREDENTIALS='{"corp":{"email":"a@x.com","token":"..."},"paas":{"email":"b@x.com","token":"..."}}'
//
// One var per credential kind, not one pair per connection — this scales to
// any number of configured connections without a new env var per one. Must
// be a named struct wrapping the map (not a bare map[string]T): Kong checks
// for a json.Unmarshaler implementation by type before falling back to its
// own built-in map decoder (which expects key=value;key2=value2 syntax, not
// JSON), and a bare map type never satisfies json.Unmarshaler.
type JiraCredentials struct {
	byName map[string]jiraCredential
}

// UnmarshalJSON implements json.Unmarshaler so Kong decodes this type from
// its env var automatically.
func (c *JiraCredentials) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &c.byName)
}

// jiraClientForProject resolves the Jira connection covering projectKey and
// constructs a client against it, using the credential registered under
// that connection's Name in UNJIRA_JIRA_CREDENTIALS.
func (a *appContext) jiraClientForProject(projectKey string) (*jira.Client, error) {
	conn, ok := a.config.JiraConnectionForProject(projectKey)
	if !ok {
		return nil, fmt.Errorf("no configured jira connection covers project %q", projectKey)
	}

	creds, ok := a.jiraCredentials.byName[conn.Name]
	if !ok {
		return nil, fmt.Errorf(
			"no credentials for jira connection %q in UNJIRA_JIRA_CREDENTIALS", conn.Name,
		)
	}

	return jira.New(conn.Site, creds.Email, creds.Token)
}

// taskTracker resolves the configured tracker backend for projectKey. No
// command calls this yet (no phase-1 commands exist) — it's the seam
// phase-1's future commands hang off of.
func (a *appContext) taskTracker(projectKey string) (tasktracker.TaskTracker, error) {
	switch a.config.TrackerBackend() {
	case "jira":
		client, err := a.jiraClientForProject(projectKey)
		if err != nil {
			return nil, err
		}

		return jira.NewTracker(client), nil
	case "local":
		return local.New(a.store), nil
	default:
		return nil, fmt.Errorf("unknown tracker backend %q", a.config.Tracker.Backend)
	}
}

// projectKey resolves --project, falling back to the first configured
// project key.
func (a *appContext) projectKey(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if len(a.config.Jira) > 0 && len(a.config.Jira[0].ProjectKeys) > 0 {
		return a.config.Jira[0].ProjectKeys[0], nil
	}

	return "", fmt.Errorf("no project key: pass --project or set jira[].project_keys in config")
}

// llmClient builds the configured LLM client, validating the config and the
// credential first so a misconfiguration fails before any collector runs or
// any lease is taken.
//
// The key comes from the environment (UNJIRA_LLM_API_KEY), never a config
// file — the same rule UNJIRA_JIRA_CREDENTIALS follows.
func (a *appContext) llmClient() (llm.Client, error) {
	if err := a.config.LLM.Validate(); err != nil {
		return nil, err
	}
	// LLMConfig.Validate covers Model and ContextWindowTokens but not BaseURL
	// (verified in internal/config/config.go). An empty base URL would send
	// unjira's prompts to the SDK's own default endpoint — api.openai.com —
	// which is both wrong and a credential-leak risk when the configured
	// backend was meant to be a local gateway. Fail loudly instead.
	if a.config.LLM.BaseURL == "" {
		return nil, fmt.Errorf("llm.base_url is required: refusing to fall back to the SDK's default endpoint")
	}
	if a.llmAPIKey == "" {
		return nil, fmt.Errorf("UNJIRA_LLM_API_KEY is not set: the LLM backend needs a credential")
	}

	return openai.New(a.config.LLM.BaseURL, a.llmAPIKey, a.config.LLM.Model), nil
}

type collectCmd struct{}

func (c *collectCmd) Run(app *appContext) error {
	linkExclusions, err := app.config.CompiledLinkExclusions()
	if err != nil {
		return err
	}

	results, err := pipeline.RunCollect(app.config, app.store, registry, linkExclusions)
	if err != nil {
		return err
	}

	for name, count := range results {
		if count < 0 {
			fmt.Printf("%s: enabled in config but no such collector is registered\n", name)
		} else {
			fmt.Printf("%s: %d new event(s)\n", name, count)
		}
	}

	return nil
}

type digestCmd struct {
	Date string `help:"Day to digest, YYYY-MM-DD (default: today)."`
}

func (c *digestCmd) Run(app *appContext) error {
	day := time.Now().UTC()
	if c.Date != "" {
		parsed, err := time.Parse("2006-01-02", c.Date)
		if err != nil {
			return fmt.Errorf("invalid --date %q: %w", c.Date, err)
		}
		day = parsed
	}

	out, err := pipeline.RenderDigest(app.store, day)
	if err != nil {
		return err
	}

	fmt.Println(out)
	return nil
}

type statusCmd struct{}

func (c *statusCmd) Run(app *appContext) error {
	counts, err := app.store.EventCountsBySource()
	if err != nil {
		return err
	}
	if len(counts) == 0 {
		fmt.Println("No events yet. Run: unjira collect")
		return nil
	}

	fmt.Println("Events by source:")
	for _, row := range counts {
		fmt.Printf("  %s: %d (latest %s)\n", row.Source, row.Count, row.Latest)
	}

	cursorCounts, err := app.store.CursorCounts()
	if err != nil {
		return err
	}

	fmt.Println("Cursors:")
	for _, row := range cursorCounts {
		fmt.Printf("  %s: %d tracked resource(s), updated %s\n", row.Collector, row.Count, row.Latest)
	}

	return nil
}

type devSeedCmd struct {
	Project string `help:"Project key (default: first in config)."`
	Count   int    `default:"6" help:"Number of issues to seed."`
}

func (c *devSeedCmd) Run(app *appContext) error {
	projectKey, err := app.projectKey(c.Project)
	if err != nil {
		return err
	}

	client, err := app.jiraClientForProject(projectKey)
	if err != nil {
		return err
	}

	keys, err := devtools.Seed(client, projectKey, c.Count)
	if err != nil {
		return err
	}

	fmt.Printf("Seeded %d issue(s): %s\n", len(keys), strings.Join(keys, ", "))
	return nil
}

type devResetCmd struct {
	Project string `help:"Project key (default: first in config)."`
}

func (c *devResetCmd) Run(app *appContext) error {
	projectKey, err := app.projectKey(c.Project)
	if err != nil {
		return err
	}

	client, err := app.jiraClientForProject(projectKey)
	if err != nil {
		return err
	}

	keys, err := devtools.Reset(client, projectKey)
	if err != nil {
		return err
	}

	if len(keys) > 0 {
		fmt.Printf("Deleted %d seeded issue(s): %s\n", len(keys), strings.Join(keys, ", "))
	} else {
		fmt.Printf("Deleted %d seeded issue(s)\n", len(keys))
	}

	return nil
}

type devWorkflowCmd struct {
	Project string `help:"Project key (default: first in config)."`
}

func (c *devWorkflowCmd) Run(app *appContext) error {
	projectKey, err := app.projectKey(c.Project)
	if err != nil {
		return err
	}

	client, err := app.jiraClientForProject(projectKey)
	if err != nil {
		return err
	}

	graph, err := workflow.MineProject(client, projectKey, 200)
	if err != nil {
		return err
	}

	fmt.Println("Statuses:")
	categories := graph.StatusCategories()
	names := sortedKeys(categories)
	for _, name := range names {
		fmt.Printf("  %s [%s]\n", name, categories[name])
	}

	fmt.Println("Observed transitions:")
	edges := graph.Edges()
	for _, key := range sortedEdgeKeys(edges) {
		fmt.Printf("  %s -> %s  (x%d)\n", key[0], key[1], edges[key])
	}

	return nil
}

// devNarrateCmd runs one collect -> Cluster -> Persist pass under the
// pipeline lease and prints the narratives it produced — the first real
// exercise of slice 3's machinery against a real event history and a real
// LLM. See devWorkflowCmd for the same "run a real stage and show me the
// output" shape.
type devNarrateCmd struct {
	Since  config.Span `default:"24h" help:"How far back to narrate (e.g. 36h, 7d, 2w, 7d12h)."`
	DryRun bool        `help:"Run the full pass, including real LLM calls, but persist nothing."`
}

// Run assembles the LLM client and validates config before doing any
// collector or store work, so a misconfiguration (missing credential, bad
// correlator config) fails immediately rather than after collectors have
// already run or the pipeline lease has been taken.
func (c *devNarrateCmd) Run(app *appContext) error {
	client, err := app.llmClient()
	if err != nil {
		return err
	}
	if err := app.config.Correlator.Validate(); err != nil {
		return err
	}

	linkExclusions, err := app.config.CompiledLinkExclusions()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runID := fmt.Sprintf("dev-narrate-%d", os.Getpid())
	if err := app.acquireNarrateLease(ctx, runID); err != nil {
		return err
	}
	defer app.releaseNarrateLease(runID)

	if _, err := pipeline.RunCollect(app.config, app.store, registry, linkExclusions); err != nil {
		return err
	}

	window := c.window()

	result, err := pipeline.RunNarrate(ctx, app.store, client, app.config, window,
		pipeline.NarrateOptions{DryRun: c.DryRun})
	if err != nil {
		return err
	}

	fmt.Print(pipeline.RenderNarrateResult(result))

	return nil
}

// window returns the [now-Since, now) range to narrate, in UTC.
func (c *devNarrateCmd) window() correlator.TimeRange {
	now := time.Now().UTC()

	return correlator.TimeRange{Start: now.Add(-c.Since.Duration()), End: now}
}

// acquireNarrateLease blocks until the pipeline lock is free, so a
// concurrent pass waits rather than fails.
func (a *appContext) acquireNarrateLease(ctx context.Context, runID string) error {
	return a.store.Acquire(ctx, runID, time.Now, narrateLeaseTTL, narrateLeasePoll)
}

// releaseNarrateLease releases the pipeline lock, logging any error rather
// than returning it — a release failure must never mask a real failure from
// the pass itself, which by this point has already returned (or is about
// to).
func (a *appContext) releaseNarrateLease(runID string) {
	if err := a.store.ReleaseLock(runID); err != nil {
		log.Printf("releasing pipeline lock: %v", err)
	}
}

type devCmd struct {
	Seed     devSeedCmd     `cmd:"" help:"Create labeled test issues and generate changelog history."`
	Reset    devResetCmd    `cmd:"" help:"Delete every seed-labeled issue in the project."`
	Workflow devWorkflowCmd `cmd:"" help:"Mine and print the observed workflow graph for a project."`
	Narrate  devNarrateCmd  `cmd:"" help:"Run one collect+cluster+persist pass and print the narratives."`
}

var cli struct {
	Config          string          `help:"Path to unjira.config.json (default: ./unjira.config.json)."`
	JiraCredentials JiraCredentials `env:"UNJIRA_JIRA_CREDENTIALS" help:"JSON object mapping connection name to {email, token}."`
	LLMAPIKey       string          `name:"llm-api-key" env:"UNJIRA_LLM_API_KEY" help:"API key for the LLM backend."`

	Collect collectCmd `cmd:"" help:"Run every enabled collector and persist new events."`
	Digest  digestCmd  `cmd:"" help:"Print the drift digest for a day."`
	Status  statusCmd  `cmd:"" help:"Event counts and collector cursor freshness."`
	Dev     devCmd     `cmd:"" help:"Tools for the dev Jira instance (seed/reset test data, inspect workflows)."`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// run holds everything that must close (the store) before main exits, so
// os.Exit never bypasses a deferred close.
func run() error {
	ctx := kong.Parse(&cli,
		kong.Name("unjira"),
		kong.Description("A reconciliation agent that keeps Jira in sync with what you actually did."),
	)

	cfg, err := config.Load(cli.Config)
	if err != nil {
		return err
	}

	s, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	return ctx.Run(&appContext{
		config:          cfg,
		store:           s,
		jiraCredentials: cli.JiraCredentials,
		llmAPIKey:       cli.LLMAPIKey,
	})
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedEdgeKeys(m map[[2]string]int) [][2]string {
	out := make([][2]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}
