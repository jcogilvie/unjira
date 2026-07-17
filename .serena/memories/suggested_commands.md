# Suggested commands (unjira)

Use `uv run` for everything — it's a plain argv (no `source` re-prompt from the harness) and
keeps the env synced to pyproject.toml on each run.

- Offline test suite (what CI runs): `uv run --extra dev pytest -q`
- Single test file: `uv run --extra dev pytest tests/test_refs.py -q`
- Lint: `uv run --extra dev ruff check src tests`
- Live Jira tier (needs UNJIRA_LIVE=1 + creds): `UNJIRA_LIVE=1 uv run --extra dev pytest -m live`
- CLI: `uv run unjira collect | digest | status`

Avoid `source .venv/bin/activate` — the harness can't allowlist it (arbitrary shell eval),
so it prompts every time. `.venv/bin/python -m pytest` also works but doesn't re-sync deps.

Env: no Jira creds configured locally; production code must not depend on live Jira. The
Atlassian MCP is read-only and for operator exploration only.
