"""Collector for Claude Code session transcripts.

Sessions live as JSONL under ~/.claude/projects/<project-slug>/<session-id>.jsonl
and grow as the session continues. Each changed file yields one snapshot event
(external_id includes the file size, so a session that grows produces a new
snapshot; identical re-reads dedupe at insert).

This collector is deliberately deterministic: it extracts metadata, ticket-key
candidates, and the opening ask. Judging what the session *meant* is the
correlator's job.
"""

from __future__ import annotations

import json
import os
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Iterable, Iterator

from ..events import Event, extract_ticket_keys
from ..store import Store

DEFAULT_BACKFILL_DAYS = 14


class ClaudeCodeCollector:
    name = "claude_code"

    def collect(self, store: Store, options: dict[str, Any]) -> Iterable[Event]:
        root = Path(options.get("transcript_root") or Path.home() / ".claude" / "projects")
        if not root.is_dir():
            return
        backfill_days = int(options.get("backfill_days", DEFAULT_BACKFILL_DAYS))
        horizon = datetime.now(timezone.utc) - timedelta(days=backfill_days)
        exclude_cwds = [_normalize_dir(p) for p in options.get("exclude_cwds", [])]

        for path in sorted(root.glob("*/*.jsonl")):
            stat = path.stat()
            position = f"{stat.st_mtime_ns}:{stat.st_size}"
            resource = str(path)
            if store.get_cursor(self.name, resource) == position:
                continue
            mtime = datetime.fromtimestamp(stat.st_mtime, tz=timezone.utc)
            if mtime < horizon:
                store.set_cursor(self.name, resource, position)
                continue

            event = self._session_event(path, mtime, exclude_cwds)
            store.set_cursor(self.name, resource, position)
            if event is not None:
                yield event

    def _session_event(
        self, path: Path, mtime: datetime, exclude_cwds: list[str] | None = None
    ) -> Event | None:
        session_id = path.stem
        cwd = git_branch = None
        first_ts = last_ts = None
        user_texts: list[str] = []
        keys: dict[str, None] = {}

        for line in _jsonl_lines(path):
            cwd = line.get("cwd") or cwd
            git_branch = line.get("gitBranch") or git_branch
            ts = line.get("timestamp")
            if ts:
                first_ts = first_ts or ts
                last_ts = ts
            text = _message_text(line)
            if text:
                for key in extract_ticket_keys(text):
                    keys.setdefault(key)
                if line.get("type") == "user":
                    user_texts.append(text)

        if not user_texts:
            return None
        if cwd and _is_excluded(cwd, exclude_cwds or []):
            return None  # unjira's own repo etc. — skip to avoid self-reference loops
        if git_branch:
            for key in extract_ticket_keys(git_branch):
                keys.setdefault(key)

        project = os.path.basename(cwd) if cwd else path.parent.name
        opening = user_texts[0].replace("\n", " ").strip()
        if len(opening) > 160:
            opening = opening[:157] + "..."
        branch_note = f" on branch {git_branch}" if git_branch else ""
        summary = (
            f"Claude Code session in {project}{branch_note}: "
            f'{len(user_texts)} user messages. Opened with: "{opening}"'
        )

        return Event(
            source=self.name,
            external_id=f"{session_id}:{path.stat().st_size}",
            occurred_at=_parse_ts(last_ts) or mtime,
            summary=summary,
            artifacts={
                "session_id": session_id,
                "cwd": cwd,
                "git_branch": git_branch,
                "ticket_keys": list(keys),
                "user_message_count": len(user_texts),
                "started_at": first_ts,
            },
            raw_ref=str(path),
        )


def _normalize_dir(path: str) -> str:
    """Absolute, symlink-resolved directory string without a trailing separator."""
    return os.path.normpath(os.path.abspath(os.path.expanduser(path)))


def _is_excluded(cwd: str, exclude_cwds: list[str]) -> bool:
    """True if cwd equals or is nested under any excluded prefix (path-boundary aware).

    Uses os.path.commonpath rather than str.startswith so that a sibling sharing a string
    prefix (e.g. ``/w/unjira-docs`` vs excluded ``/w/unjira``) is NOT excluded.
    """
    target = _normalize_dir(cwd)
    for prefix in exclude_cwds:
        try:
            if os.path.commonpath([target, prefix]) == prefix:
                return True
        except ValueError:
            continue  # different drives / mixed abs+rel — not comparable, so not excluded
    return False


def _jsonl_lines(path: Path) -> Iterator[dict[str, Any]]:
    with path.open(encoding="utf-8", errors="replace") as fh:
        for raw in fh:
            raw = raw.strip()
            if not raw:
                continue
            try:
                line = json.loads(raw)
            except json.JSONDecodeError:
                continue
            if isinstance(line, dict):
                yield line


def _message_text(line: dict[str, Any]) -> str:
    """Human-authored or model-authored text in a transcript line.

    Tool results and command wrappers (content starting with '<') are skipped —
    they are plumbing, not narrative.
    """
    if line.get("type") not in ("user", "assistant"):
        return ""
    content = (line.get("message") or {}).get("content")
    parts: list[str] = []
    if isinstance(content, str):
        parts.append(content)
    elif isinstance(content, list):
        for block in content:
            if isinstance(block, dict) and block.get("type") == "text":
                parts.append(block.get("text") or "")
    text = "\n".join(p for p in parts if p).strip()
    return "" if text.startswith("<") else text


def _parse_ts(value: str | None) -> datetime | None:
    if not value:
        return None
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
