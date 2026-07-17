# Working on unjira

Guidance for anyone (human or agent) editing this codebase. This is distinct from `rules/`:

- **`CLAUDE.md` (this file)** — how to work on *unjira the codebase*.
- **`rules/`** — behavioral constraints for the agent *unjira implements* (the correlator and
  reconciler load them into their prompts). Don't put codebase conventions there, or agent
  behavioral norms here.
- **`docs/design-notes.md`** — *why* unjira is shaped this way: the failure modes that drove the
  architecture. Read it before changing the pipeline's structure.

## What unjira is

A reconciliation agent that diffs reality (what you did, from event streams) against Jira (what the
org thinks you did) and proposes the minimal patch. It is a reconciler in the Kubernetes-controller
sense, not an event forwarder. See `README.md` for the pipeline and locked design decisions.

## Commands

Use `uv run` for everything — it keeps the environment synced to `pyproject.toml` on each run.

```sh
uv run --extra dev pytest -q          # offline test tiers (what CI runs); 3 live tests deselected
uv run --extra dev ruff check src tests
uv run unjira collect | digest | status
UNJIRA_LIVE=1 uv run --extra dev pytest -m live   # writes to the dev Jira instance; needs creds
```

## Architecture invariants

These are load-bearing — `docs/design-notes.md` explains the incidents behind each:

- **Collectors are dumb and deterministic.** They extract metadata + candidates and defer all
  judgment. No LLM, no network beyond their own source, no "does this ticket exist" checks. Adding
  a stream = implement the `Collector` protocol, register it, enable it in config.
- **All verification lives in the reconciler.** Never emit a state-bearing action from transcript
  intent; confirm current state against live Jira/GitHub first (see `rules/intent-not-outcome.md`).
- **The correlator's deterministic primitives run before any model.** `correlator/refs.py` and
  `correlator/fanout.py` are pure functions with no I/O and no Jira dependency — keep them that way;
  they're what keeps the LLM's review queue signal-rich.
- **Never silently drop data.** Prefer erroring loudly over truncating (e.g. `refs.py` raises on an
  over-`max_span` range). Silent data loss is the hardest class of bug to notice here.
- **Writes are gated by the pipeline, not the client.** `JiraClient` has write methods, but
  *authorization* to call them lives in the review queue / autonomy graduation, never in the client.

## Conventions

- TDD: write the failing test first, then the implementation. Tests are plain `pytest` functions
  with direct assertions (see `tests/`), offline by default; live tests are marked `@pytest.mark.live`.
- `from __future__ import annotations` at the top of every module; modern type hints.
- Module docstrings tie the code to the lesson or design decision it implements — match that style.
- Credentials come from the environment (`UNJIRA_JIRA_*`), never config files. Config is
  `unjira.config.json` (gitignored); `config/unjira.example.json` is the template.
- The Jira facade over `atlassian-python-api` is the seam: keep divergence trivial so the community
  absorbs API churn.
