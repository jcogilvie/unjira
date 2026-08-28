// Package main implements the unjira CLI: collect | digest | status | watch | dev.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/clients/local"
	"github.com/jcogilvie/unjira/internal/clients/openai"
	"github.com/jcogilvie/unjira/internal/collector/claudecode"
	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/credentials"
	"github.com/jcogilvie/unjira/internal/devtools"
	"github.com/jcogilvie/unjira/internal/envfile"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
	"github.com/jcogilvie/unjira/internal/workflow"
)

const (
	// pipelineLeaseTTL bounds how long a crashed pass (dev narrate or watch)
	// can hold the pipeline lock before another run may steal it. Generous
	// relative to a pass (minutes), since stealing a live lease is worse
	// than waiting.
	//
	// Named pipelineLease*, not narrateLease*: originally sized for
	// devNarrateCmd's single manual pass, this now also bounds each of
	// watch's looped passes (collect+narrate+match+reconcile+auto-commit)
	// under one lease per tick — see acquirePipelineLease's doc comment.
	pipelineLeaseTTL = 30 * time.Minute
	// pipelineLeasePoll is how often a blocked Acquire retries.
	pipelineLeasePoll = 2 * time.Second
)

// registry maps collector names to factories, mirroring
// internal/collectors.REGISTRY in the Python implementation.
var registry = map[string]func() pipeline.Collector{
	"claude_code": func() pipeline.Collector { return claudecode.New() },
	"jira":        func() pipeline.Collector { return collectorjira.New() },
}

// appContext carries the loaded config, open store, and Jira credentials to
// every command.
type appContext struct {
	config          config.Config
	store           *store.Store
	jiraCredentials credentials.JSONSet
	llmAPIKey       string
}

// jiraClientForProject resolves the Jira connection covering projectKey and
// constructs a client against it, using the credential registered under
// that connection's Name in credentials.EnvVar.
func (a *appContext) jiraClientForProject(projectKey string) (*jira.Client, error) {
	conn, ok := a.config.JiraConnectionForProject(projectKey)
	if !ok {
		return nil, fmt.Errorf("no configured jira connection covers project %q", projectKey)
	}

	creds, ok := a.jiraCredentials.Set().For(conn.Name)
	if !ok {
		return nil, fmt.Errorf(
			"no credentials for jira connection %q in %s", conn.Name, credentials.EnvVar,
		)
	}

	return jira.New(conn.Site, creds.Email, creds.Token)
}

// taskTracker resolves the configured tracker backend for projectKey.
// devNarrateCmd is its first caller, resolving a single tracker from the
// default project for its matching pass — see the call site for the
// multi-connection limitation that implies.
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
// The credential itself never comes from a config file — the same rule
// UNJIRA_JIRA_CREDENTIALS follows. It is either UNJIRA_LLM_API_KEY, or the
// stdout of llm.api_key_helper, which is a path rather than a secret.
//
// Precedence is helper-first: it is the more specific instruction, and it is the
// only one of the two that can outlive a token expiring mid-pass.
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
	credential, err := a.llmCredential()
	if err != nil {
		return nil, err
	}

	return openai.New(a.config.LLM.BaseURL, credential,
		a.config.LLM.Model, a.config.LLM.MaxOutputTokens), nil
}

// llmCredential resolves the configured credential source: the helper if one is
// configured, else the static environment key.
//
// Note what is deliberately NOT done here: the helper is not executed to prove
// it works. Doing so would double every invocation's cost on the common path,
// and the helper is run before the first completion anyway — so a broken helper
// still fails before any real work, just one step later.
func (a *appContext) llmCredential() (llm.CredentialSource, error) {
	helper, err := a.config.LLM.ResolvedAPIKeyHelper()
	if err != nil {
		return nil, err
	}

	if helper != "" {
		return llm.NewHelperCredential(helper), nil
	}

	if a.llmAPIKey == "" {
		return nil, fmt.Errorf(
			"no LLM credential: set UNJIRA_LLM_API_KEY, or llm.api_key_helper in config " +
				"if the backend issues short-lived tokens",
		)
	}

	return llm.StaticCredential(a.llmAPIKey), nil
}

type collectCmd struct{}

func (c *collectCmd) Run(app *appContext) error {
	linkExclusions, err := app.config.CompiledLinkExclusions()
	if err != nil {
		return err
	}

	results, err := pipeline.RunCollect(app.config, app.store, registry, linkExclusions, app.jiraCredentials.Set())
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
	// Validated here rather than inside RunReconcile alone, so a bad reconciler
	// config fails before any collector work rather than after it.
	if err := app.config.Reconciler.Validate(); err != nil {
		return err
	}

	linkExclusions, err := app.config.CompiledLinkExclusions()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runID := fmt.Sprintf("dev-narrate-%d", os.Getpid())
	if err := app.acquirePipelineLease(ctx, runID); err != nil {
		return err
	}
	defer app.releasePipelineLease(runID)

	if _, err := pipeline.RunCollect(app.config, app.store, registry, linkExclusions, app.jiraCredentials.Set()); err != nil {
		return err
	}

	window := c.window()

	result, err := pipeline.RunNarrate(ctx, app.store, client, app.config, window,
		pipeline.NarrateOptions{DryRun: c.DryRun})
	if err != nil {
		return err
	}

	fmt.Print(pipeline.RenderNarrateResult(result))

	if c.DryRun {
		// Matching writes narrative_issues rows and may set
		// narratives.issue_key, so it has no dry-run mode: skipping is
		// stated rather than silent, or the operator is left wondering why
		// nothing matched. Reconcile is skipped with it, since it has nothing
		// to work from until matching has linked something.
		fmt.Println("matching skipped (--dry-run)")
		fmt.Println("reconcile skipped (--dry-run)")

		return nil
	}

	// Matching uses ONE tracker, resolved from the default project. That is a
	// real limitation on a multi-connection jira setup: a candidate key
	// belonging to a different JiraConnection will be verified against the
	// wrong site and reported unresolved. store.NarrativeIssue.Connection
	// already records which connection each candidate came from, so resolving
	// a tracker per candidate is a contained future change. The local backend
	// ignores projectKey entirely, so it is unaffected.
	project, err := app.projectKey("")
	if err != nil {
		return err
	}

	tracker, err := app.taskTracker(project)
	if err != nil {
		return err
	}

	matchResult, matchErr := pipeline.RunMatch(ctx, app.store, tracker, client, app.config)

	// Render before returning the error: RunMatch isolates failures per
	// narrative, so the healthy narratives matched and the operator should see
	// them alongside whatever failed.
	fmt.Print(pipeline.RenderMatchResult(matchResult))

	if matchErr != nil {
		return fmt.Errorf("matching narratives to issues: %w", matchErr)
	}

	// Reconcile only after a clean matching pass: it reconciles against
	// matching's link set, so running it after a failed pass would draft
	// against a half-updated one.
	//
	// It reuses the tracker matching resolved, so the single-tracker
	// limitation described above applies identically here — a link whose
	// Connection differs from this one is verified against the wrong site.
	reconcileResult, reconcileErr := pipeline.RunReconcile(
		ctx, app.store, tracker, client, app.config, pipeline.ReconcileOptions{})

	// Render before returning the error, matching the matching stage above:
	// Reconcile isolates failures per narrative, so healthy narratives produced
	// proposals the operator should see alongside whatever failed.
	fmt.Print(pipeline.RenderReconcileResult(reconcileResult))

	if reconcileErr != nil {
		return fmt.Errorf("reconciling narratives: %w", reconcileErr)
	}

	return nil
}

// window returns the [now-Since, now) range to narrate, in UTC.
func (c *devNarrateCmd) window() correlator.TimeRange {
	now := time.Now().UTC()

	return correlator.TimeRange{Start: now.Add(-c.Since.Duration()), End: now}
}

// acquirePipelineLease blocks until the pipeline lock is free, so a
// concurrent pass waits rather than fails.
//
// Shared by devNarrateCmd (one pass, one lease for the whole command) and
// watchCmd (one lease PER loop iteration — see watchLoop/runWatchPass): both
// callers span collect+narrate+match+reconcile under a single lease, which
// is exactly why RunNarrate/RunMatch/RunReconcile each deliberately take no
// lease of their own.
func (a *appContext) acquirePipelineLease(ctx context.Context, runID string) error {
	return a.store.Acquire(ctx, runID, time.Now, pipelineLeaseTTL, pipelineLeasePoll)
}

// releasePipelineLease releases the pipeline lock, logging any error rather
// than returning it — a release failure must never mask a real failure from
// the pass itself, which by this point has already returned (or is about
// to).
func (a *appContext) releasePipelineLease(runID string) {
	if err := a.store.ReleaseLock(runID); err != nil {
		log.Printf("releasing pipeline lock: %v", err)
	}
}

type devCmd struct {
	Seed     devSeedCmd     `cmd:"" help:"Create labeled test issues and generate changelog history."`
	Reset    devResetCmd    `cmd:"" help:"Delete every seed-labeled issue in the project."`
	Workflow devWorkflowCmd `cmd:"" help:"Mine and print the observed workflow graph for a project."`
	Narrate  devNarrateCmd  `cmd:"" help:"Run one collect+narrate+match+reconcile pass and print what it found and proposed."`
}

// watchCmd is unjira's headline phase-1 command: the long-running loop that
// makes unjira a thing that runs rather than a thing you run. Per the
// phase-1 spec's command surface
// (docs/superpowers/specs/2026-08-11-phase1-correlator-design.md): interval-
// driven, no human runs it directly — cron/launchd/a cloud scheduler calls
// this repeatedly (with --once), or it loops on its own --interval.
//
// It composes exactly the stages devNarrateCmd was prototyping —
// collect -> narrate -> match -> reconcile — plus the one thing dev narrate
// deliberately never did: the auto-commit gate (internal/gate). Dev narrate
// exists to inspect a pass; watch exists to act on one unattended, which is
// why the gate belongs here and nowhere earlier. See
// docs/superpowers/specs/2026-08-26-watch-autocommit-design.md.
type watchCmd struct {
	Interval config.Span `default:"5m" help:"How often to run a pass (e.g. 30s, 5m, 1h)."`
	// Since is deliberately independent of Interval, not derived from it: a
	// generous margin tolerates a missed tick (a slow pass, a restart)
	// without narrating anything twice — UnlinkedEventsInRange excludes
	// every event already linked to a narrative regardless of how wide the
	// window is, so widening Since is always safe, never duplicative. A
	// cursor-based "since last successful run" watermark (per the phase-1
	// spec's "Watermarks" section) would be tighter, but is deliberately
	// left for a later slice — this fixed lookback is simpler and the
	// dedup-by-window-membership property above is what makes it safe to
	// ship as-is rather than a placeholder that must be revisited.
	Since  config.Span `default:"1h" help:"How far back each pass narrates (e.g. 1h, 24h)."`
	Once   bool        `help:"Run a single pass and exit, instead of looping. Makes the loop testable and cron-friendly without a supervisor."`
	DryRun bool        `help:"Run every stage's real work, including LLM calls, but skip Persist and the auto-commit gate. Never writes to the store or a real tracker."`
}

// Run validates configuration once, up front — before the first lease, let
// alone the first pass — because an error that will recur forever (invalid
// config, a missing LLM credential) must fail fast rather than retrying on
// every tick for no reason. A transient failure (a Jira 503, an expired
// token) is exactly what the per-pass handling in watchLoop exists to
// survive instead; see watchLoop and runWatchPass for that split.
func (c *watchCmd) Run(app *appContext) error {
	client, err := app.llmClient()
	if err != nil {
		return err
	}
	if err := app.config.Correlator.Validate(); err != nil {
		return err
	}
	if err := app.config.Reconciler.Validate(); err != nil {
		return err
	}
	if err := app.config.Match.Validate(); err != nil {
		return err
	}
	// AutoCommitRule.Validate is otherwise only ever invoked implicitly via
	// gate.Decide's map lookup, which never rejects a malformed rule — it
	// just mis-gates silently (see AutoCommitRule.Validate's own doc
	// comment on why a floor outside [0,1] is a config error rather than a
	// value gate.Decide can operate on "safely"). Validating here is the
	// only place this ever actually happens before a real pass runs.
	for actionType, rule := range app.config.AutoCommit {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("auto_commit rule for %q: %w", actionType, err)
		}
	}

	// Write scope's startup layer (see
	// docs/superpowers/specs/2026-08-27-write-scope-design.md, "Choke point:
	// gate.Applier, plus startup validation"): writable_project_keys must be
	// a subset of project_keys for every connection — a writable-but-
	// unreachable project would otherwise load silently and only surface
	// whenever a write against it was actually attempted.
	for _, conn := range app.config.Jira {
		if err := conn.ValidateWriteScope(); err != nil {
			return err
		}
	}
	// The second half: tracker.default_project, since it's static config
	// known entirely at startup, must ALSO be writable — not merely
	// resolvable to a connection (JiraConnectionForProject) — or `watch`
	// would run for however long it takes a `create` action to actually
	// fire before this misconfiguration surfaces. Skipped when unset: an
	// operator who never expects a `create` action need not configure a
	// default project at all (the existing, unrelated "no
	// tracker.default_project configured" error only matters once a
	// `create` action is actually attempted — see Applier.applyCreate).
	if app.config.Tracker.DefaultProject != "" {
		if _, err := app.config.DefaultProjectConnection(); err != nil {
			return err
		}
	}

	linkExclusions, err := app.config.CompiledLinkExclusions()
	if err != nil {
		return err
	}

	// Matching/reconciling/applying all use ONE tracker, resolved from the
	// default project — the same multi-connection limitation
	// devNarrateCmd.Run's own doc comment names, unchanged here.
	project, err := app.projectKey("")
	if err != nil {
		return err
	}

	tracker, err := app.taskTracker(project)
	if err != nil {
		return err
	}

	applier := gate.NewApplier(app.store, tracker, app.config.Tracker.DefaultProject, app.config.Jira)

	// ctx governs the LOOP — whether to acquire another lease, whether to
	// keep waiting out the interval — but deliberately does NOT govern an
	// in-flight PASS: watchLoop derives a non-cancelable context per pass via
	// context.WithoutCancel, so a SIGINT/SIGTERM lets the current pass finish
	// (an in-flight LLM call or tracker write completes) rather than
	// aborting it partway. That is the "graceful" half of graceful shutdown;
	// killing the process outright is separately safe regardless (the lease
	// is TTL-bounded and Persist is transactional), per the design doc.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	since := c.Since.Duration()

	return app.watchLoop(ctx, c.Interval.Duration(), c.Once, func(passCtx context.Context, _ string) error {
		return app.runWatchPass(passCtx, client, tracker, applier, linkExclusions, since, c.DryRun)
	})
}

// watchLoop owns the interval loop's mechanics — per-tick lease acquisition,
// --once's single-pass exit, graceful shutdown, and "a pass failure must not
// exit the loop" — independent of what a pass actually does. Kept separate
// from watchCmd.Run and runWatchPass so a test can exercise the LOOP with a
// fake runOnePass, and exercise a real pass's pipeline composition
// separately, without needing both working at once.
//
// Each tick acquires its OWN lease (a fresh runID), rather than one lease
// held for the loop's entire lifetime: RunNarrate/RunMatch/RunReconcile each
// deliberately take no lease so a caller can span them under ONE PASS's
// lease — not under watch's entire, potentially-unbounded runtime. Holding a
// single lease across the whole process would also mean a crashed watch
// process leaves the lock held for the full pipelineLeaseTTL regardless of
// how long it had actually been idle between ticks, defeating the TTL's
// purpose (bounding a CRASHED PASS, not a long-running watch).
func (a *appContext) watchLoop(
	ctx context.Context,
	interval time.Duration,
	once bool,
	runOnePass func(ctx context.Context, runID string) error,
) error {
	for {
		if ctx.Err() != nil {
			return nil
		}

		runID := fmt.Sprintf("watch-%d-%d", os.Getpid(), time.Now().UnixNano())

		if err := a.acquirePipelineLease(ctx, runID); err != nil {
			if ctx.Err() != nil {
				// Canceled while blocked waiting for a contended lease: this
				// is shutdown, not a pass failure to log-and-retry.
				return nil
			}
			// Any other acquire failure (a transient SQLite error) is logged
			// and retried next tick — the same treatment as a pass failure
			// below, since a lease that cannot be acquired right now is
			// exactly the kind of transient condition watch exists to
			// survive rather than exit over.
			log.Printf("watch: acquiring pipeline lease: %v", err)
		} else {
			passErr := runOnePass(context.WithoutCancel(ctx), runID)
			a.releasePipelineLease(runID)

			if passErr != nil {
				log.Printf("watch: pass failed: %v", passErr)
			}
		}

		if once {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// runWatchPass runs one collect -> narrate -> match -> reconcile ->
// auto-commit pass, under the lease watchLoop already holds for this tick.
//
// --dry-run stops after narrate and SAYS so on every stage it skips: narrate's
// own DryRun option already skips its Persist, but matching writes
// narrative_issues rows (and may set narratives.issue_key) and reconcile
// drafts against matching's link set — neither has a dry-run mode of its
// own, so running them under --dry-run would either write for real or draft
// against a link set that was never persisted. Silence here would leave an
// operator wondering why nothing happened past narration; devNarrateCmd sets
// this same precedent for matching+reconcile, and this adds auto-commit to
// the list of things named as skipped.
//
// Auto-commit runs ONLY when reconcileErr == nil — never merely because
// reconcileResult.Persisted is non-empty. RunReconcile isolates failures per
// narrative and can return a non-nil error alongside a perfectly usable
// partial result: the narratives that drafted cleanly still persisted their
// proposals (see RunReconcile's own doc comment on that asymmetry with
// Persist). Gating on "were there any rows" instead of "did the whole pass
// finish cleanly" is exactly the trap the phase-1 spec's all-or-nothing
// property exists to close — a withheld pass's actions are NOT lost, they
// sit at status=proposed for a human in triage; this only withholds the
// SEPARATE decision to apply any of them without review.
func (a *appContext) runWatchPass(
	ctx context.Context,
	client llm.Client,
	tracker tasktracker.TaskTracker,
	applier *gate.Applier,
	linkExclusions []*regexp.Regexp,
	since time.Duration,
	dryRun bool,
) error {
	if _, err := pipeline.RunCollect(a.config, a.store, registry, linkExclusions, a.jiraCredentials.Set()); err != nil {
		return err
	}

	now := time.Now().UTC()
	window := correlator.TimeRange{Start: now.Add(-since), End: now}

	narrateResult, err := pipeline.RunNarrate(ctx, a.store, client, a.config, window,
		pipeline.NarrateOptions{DryRun: dryRun})
	if err != nil {
		return err
	}
	fmt.Print(pipeline.RenderNarrateResult(narrateResult))

	if dryRun {
		fmt.Println("matching skipped (--dry-run)")
		fmt.Println("reconcile skipped (--dry-run)")
		fmt.Println("auto-commit skipped (--dry-run)")

		return nil
	}

	matchResult, matchErr := pipeline.RunMatch(ctx, a.store, tracker, client, a.config)
	fmt.Print(pipeline.RenderMatchResult(matchResult))

	if matchErr != nil {
		return fmt.Errorf("matching narratives to issues: %w", matchErr)
	}

	reconcileResult, reconcileErr := pipeline.RunReconcile(
		ctx, a.store, tracker, client, a.config, pipeline.ReconcileOptions{})
	fmt.Print(pipeline.RenderReconcileResult(reconcileResult))

	if reconcileErr != nil {
		fmt.Println("auto-commit skipped (this pass did not reconcile cleanly)")

		return fmt.Errorf("reconciling narratives: %w", reconcileErr)
	}

	autoCommitResult, autoCommitErr := pipeline.RunAutoCommit(reconcileResult.Persisted, pipeline.AutoCommitOptions{
		Rules:   a.config.AutoCommit,
		Applier: applier,
	})
	fmt.Print(pipeline.RenderAutoCommitResult(autoCommitResult))

	return autoCommitErr
}

var cli struct {
	Config          string              `help:"Path to unjira.config.json (default: ./unjira.config.json)."`
	JiraCredentials credentials.JSONSet `env:"UNJIRA_JIRA_CREDENTIALS" help:"JSON object mapping connection name to {email, token}."`
	LLMAPIKey       string              `name:"llm-api-key" env:"UNJIRA_LLM_API_KEY" help:"API key for the LLM backend."`

	Collect collectCmd `cmd:"" help:"Run every enabled collector and persist new events."`
	Digest  digestCmd  `cmd:"" help:"Print the drift digest for a day."`
	Status  statusCmd  `cmd:"" help:"Event counts and collector cursor freshness."`
	Watch   watchCmd   `cmd:"" help:"Interval loop: collect -> narrate -> match -> reconcile -> auto-commit gate."`
	Actions actionsCmd `cmd:"" help:"Machine-facing primitives over the review queue: list and decide on proposed actions."`
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
//
// envfile.Load must run before kong.Parse: Kong reads env vars (including
// UNJIRA_JIRA_CREDENTIALS and UNJIRA_LLM_API_KEY, both tagged with `env:` on
// the cli struct) at parse time, so a .env value not yet in the process
// environment at that point would never reach Kong's decoding — it would be
// silently invisible to every command, not merely to some later step.
func run() error {
	if err := envfile.Load(); err != nil {
		return err
	}

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
