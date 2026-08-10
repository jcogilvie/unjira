// Package main implements the unjira CLI: collect | digest | status | dev.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/alecthomas/kong"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/collector/claudecode"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/devtools"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/workflow"
)

// registry maps collector names to factories, mirroring
// internal/collectors.REGISTRY in the Python implementation.
var registry = map[string]func() pipeline.Collector{
	"claude_code": func() pipeline.Collector { return claudecode.New() },
}

// appContext carries the loaded config and open store to every command.
type appContext struct {
	config config.Config
	store  *store.Store
}

// jiraClient constructs a Jira client from environment credentials, per
// JiraClient.from_config in the Python implementation: UNJIRA_JIRA_SITE
// overrides config.jira.site, and UNJIRA_JIRA_EMAIL/UNJIRA_JIRA_TOKEN are
// required.
func (a *appContext) jiraClient() (*jira.Client, error) {
	site := os.Getenv("UNJIRA_JIRA_SITE")
	if site == "" {
		site = a.config.Jira.Site
	}

	email := os.Getenv("UNJIRA_JIRA_EMAIL")
	token := os.Getenv("UNJIRA_JIRA_TOKEN")
	if email == "" || token == "" {
		return nil, fmt.Errorf("set UNJIRA_JIRA_EMAIL and UNJIRA_JIRA_TOKEN (see .env.example)")
	}

	return jira.New(site, email, token)
}

// projectKey resolves --project, falling back to the first configured
// project key.
func (a *appContext) projectKey(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if len(a.config.Jira.ProjectKeys) > 0 {
		return a.config.Jira.ProjectKeys[0], nil
	}

	return "", fmt.Errorf("no project key: pass --project or set jira.project_keys in config")
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
	client, err := app.jiraClient()
	if err != nil {
		return err
	}

	projectKey, err := app.projectKey(c.Project)
	if err != nil {
		return err
	}

	keys, err := devtools.Seed(client, projectKey, c.Count)
	if err != nil {
		return err
	}

	fmt.Printf("Seeded %d issue(s): %s\n", len(keys), joinComma(keys))
	return nil
}

type devResetCmd struct {
	Project string `help:"Project key (default: first in config)."`
}

func (c *devResetCmd) Run(app *appContext) error {
	client, err := app.jiraClient()
	if err != nil {
		return err
	}

	projectKey, err := app.projectKey(c.Project)
	if err != nil {
		return err
	}

	keys, err := devtools.Reset(client, projectKey)
	if err != nil {
		return err
	}

	if len(keys) > 0 {
		fmt.Printf("Deleted %d seeded issue(s): %s\n", len(keys), joinComma(keys))
	} else {
		fmt.Printf("Deleted %d seeded issue(s)\n", len(keys))
	}

	return nil
}

type devWorkflowCmd struct {
	Project string `help:"Project key (default: first in config)."`
}

func (c *devWorkflowCmd) Run(app *appContext) error {
	client, err := app.jiraClient()
	if err != nil {
		return err
	}

	projectKey, err := app.projectKey(c.Project)
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

type devCmd struct {
	Seed     devSeedCmd     `cmd:"" help:"Create labeled test issues and generate changelog history."`
	Reset    devResetCmd    `cmd:"" help:"Delete every seed-labeled issue in the project."`
	Workflow devWorkflowCmd `cmd:"" help:"Mine and print the observed workflow graph for a project."`
}

var cli struct {
	Config string `help:"Path to unjira.config.json (default: ./unjira.config.json)."`

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

	return ctx.Run(&appContext{config: cfg, store: s})
}

func joinComma(ss []string) string {
	return strings.Join(ss, ", ")
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
