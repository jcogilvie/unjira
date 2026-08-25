# Learned rules

Corrections from the review queue get distilled into markdown rules here — one file per
rule, human-auditable and diffable. `internal/rules` (`rules.Load`) reads every file here
and hands the `scope: correlator` subset to `internal/pipeline`, which appends it to
`Cluster`'s and `Match`'s system prompts each pass — see `internal/correlator`'s
`WithClusterRules`/`WithRules` options. `scope: reconciler` and `scope: estimator` rules
are parsed and preserved by the loader (so nothing here is rejected ahead of the code that
will consume it) but are not yet wired into any prompt: `internal/reconciler` does not
exist yet, and there is no estimator.

The directory `rules.Load` reads defaults to `rules/` (this directory, relative to the
working directory) and is configurable via `rules.dir` in `unjira.config.json` — see
`config.Config.RulesDir`. A missing directory is a no-op, not an error, so a fresh clone
with no seeded rules still runs.

Keeping rules as plain markdown in the repo is deliberate: lifting team-level norms into a
shared repo later (so anybody's copy of the agent benefits) is a `git remote`, not a
redesign.

## Format

```markdown
---
scope: correlator | reconciler | estimator
confidence: high | provisional
learned: 2026-07-11
source: "review-queue correction on action #42"
---

Commits touching `auth/` belong to the SSO epic (PROJ-88), not new tickets.
```

Frontmatter is parsed as YAML, so a value containing ` #` (space then hash — YAML reads
that as starting a comment) or starting with a quote, `[`, `{`, `&`, or `*` must be
quoted, as `source` is above. This matters most for `source`: it's provenance for a human
auditing a rule (it is never rendered into a prompt — see `Render`) and isn't validated
against anything, so an unquoted value that gets silently truncated at the comment would
still look plausible and nobody would notice.

`confidence: provisional` rules are loaded and labelled in the rendered prompt, not
filtered out — the review-queue workflow that produces them needs them visible.

Nothing writes here yet — rule *proposal* generation from review-queue corrections
(`rules.Distill`) is a later phase-1 slice. Seed rules manually if you already know norms
the agent should start with; this file itself has no frontmatter and is skipped by the
loader.
