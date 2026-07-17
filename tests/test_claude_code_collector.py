"""claude_code collector: event extraction and self-exclusion (design-notes.md #11).

The collector must skip sessions whose cwd is unjira's own repo, or it would ingest its own
bookkeeping sessions as "work" and spiral. Exclusion is path-boundary aware (a sibling that
merely shares a string prefix is NOT excluded), and an excluded session still advances its
cursor so it is not rescanned every pass.
"""

from __future__ import annotations

import json
from pathlib import Path

from unjira.collectors.claude_code import ClaudeCodeCollector
from unjira.store import Store


def write_session(root: Path, project_slug: str, session_id: str, cwd: str) -> Path:
    """Create a minimal one-user-message transcript under root/<project>/<session>.jsonl."""
    proj = root / project_slug
    proj.mkdir(parents=True, exist_ok=True)
    path = proj / f"{session_id}.jsonl"
    lines = [
        {
            "type": "user",
            "cwd": cwd,
            "gitBranch": "main",
            "timestamp": "2026-07-16T12:00:00Z",
            "message": {"content": "Help me with PROJ-123 please"},
        }
    ]
    path.write_text("\n".join(json.dumps(x) for x in lines), encoding="utf-8")
    return path


def collect(tmp_path: Path, root: Path, **options):
    store = Store(tmp_path / "db.sqlite")
    opts = {"transcript_root": str(root), **options}
    events = list(ClaudeCodeCollector().collect(store, opts))
    return store, events


def test_session_yields_event_without_exclusion(tmp_path: Path):
    root = tmp_path / "projects"
    write_session(root, "proj-a", "s1", cwd="/Users/j/workspace/other")
    _, events = collect(tmp_path, root)
    assert len(events) == 1
    assert "PROJ-123" in events[0].artifacts["ticket_keys"]


def test_own_repo_session_excluded(tmp_path: Path):
    root = tmp_path / "projects"
    repo = "/Users/j/workspace/unjira"
    write_session(root, "proj-unjira", "s1", cwd=repo)
    _, events = collect(tmp_path, root, exclude_cwds=[repo])
    assert events == []


def test_nested_cwd_under_excluded_repo_is_excluded(tmp_path: Path):
    root = tmp_path / "projects"
    repo = "/Users/j/workspace/unjira"
    write_session(root, "proj-unjira", "s1", cwd=f"{repo}/src/unjira")
    _, events = collect(tmp_path, root, exclude_cwds=[repo])
    assert events == []


def test_sibling_sharing_prefix_is_not_excluded(tmp_path: Path):
    root = tmp_path / "projects"
    repo = "/Users/j/workspace/unjira"
    write_session(root, "proj-docs", "s1", cwd=f"{repo}-docs")
    _, events = collect(tmp_path, root, exclude_cwds=[repo])
    assert len(events) == 1


def test_excluded_session_still_advances_cursor(tmp_path: Path):
    root = tmp_path / "projects"
    repo = "/Users/j/workspace/unjira"
    path = write_session(root, "proj-unjira", "s1", cwd=repo)
    store, events = collect(tmp_path, root, exclude_cwds=[repo])
    assert events == []
    assert store.get_cursor("claude_code", str(path)) is not None
