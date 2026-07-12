"""Phase-0 daily digest: what happened today, and what looks untracked.

Deterministic for now — it groups events and flags the unlinked ones. The LLM
narrative pass (clustering events into work stories, matching against open
issues, drift analysis) replaces the grouping here in phase 1; the rendering
contract stays the same.
"""

from __future__ import annotations

import json
from datetime import date

from ..events import is_sentinel_key
from ..store import Store


def render_digest(store: Store, day: date) -> str:
    rows = store.events_on(day)
    lines = [f"# unjira digest — {day.isoformat()}", ""]
    if not rows:
        lines.append("No events observed.")
        return "\n".join(lines)

    linked: list[str] = []
    unlinked: list[str] = []
    for row in rows:
        artifacts = json.loads(row["artifacts"])
        keys = [k for k in artifacts.get("ticket_keys", []) if not is_sentinel_key(k)]
        stamp = row["occurred_at"][11:16] or "--:--"
        entry = f"- {stamp} [{row['source']}] {row['summary']}"
        if keys:
            linked.append(f"{entry}  ({', '.join(keys)})")
        else:
            unlinked.append(entry)

    if linked:
        lines += ["## Linked to tickets", *linked, ""]
    if unlinked:
        lines += [
            "## Untracked work (no confident ticket link)",
            *unlinked,
            "",
            "_These are candidates for a new ticket or a missed link — the main thing",
            "phase 0 is measuring. Corrections here become learned rules._",
        ]
    return "\n".join(lines)
