"""The normalized event schema — the contract every collector emits.

An Event is an observation about reality, not an instruction. Collectors are
deterministic; anything requiring judgment (clustering, matching, drafting)
happens downstream in the correlator/reconciler.
"""

from __future__ import annotations

import re
from datetime import datetime
from typing import Any

from pydantic import BaseModel, Field

# Matches PROJ-123 style keys for any project prefix. Sentinel keys —
# $PROJECT-0 / $PROJECT-1, subbed in by devs to placate commit checkers — are
# matched too; downstream treats them as an explicit "unlinked work" flag,
# not a real link.
TICKET_KEY_RE = re.compile(r"\b[A-Z][A-Z0-9]{1,9}-\d+\b")

SENTINEL_ISSUE_NUMBERS = {"0", "1"}


class Event(BaseModel):
    """A single normalized observation from one source stream."""

    source: str
    external_id: str = Field(description="Stable id within the source; (source, external_id) dedupes")
    occurred_at: datetime
    actor: str | None = None
    summary: str
    artifacts: dict[str, Any] = Field(default_factory=dict)
    raw_ref: str | None = Field(default=None, description="Pointer back to the raw source material")


def extract_ticket_keys(text: str) -> list[str]:
    """All candidate Jira keys in text, order-preserving, deduplicated."""
    seen: dict[str, None] = {}
    for match in TICKET_KEY_RE.findall(text):
        seen.setdefault(match)
    return list(seen)


def is_sentinel_key(key: str) -> bool:
    """$PROJECT-0 / $PROJECT-1 keys devs use to placate commit checkers.

    Any project prefix counts. Caveat: -0 is never a valid Jira issue, but
    $PROJECT-1 is a real issue (the project's first), so a -1 match is only
    probably a sentinel — the correlator should break the tie with a Jira
    existence check once the Jira client lands (phase 1).
    """
    return key.rsplit("-", 1)[-1] in SENTINEL_ISSUE_NUMBERS
