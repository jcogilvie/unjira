"""The collector plugin surface.

A collector reads its source since the last cursor, emits normalized Events,
and advances its cursor via the store. Collectors must be deterministic and
idempotent: re-running one is always safe because (source, external_id)
dedupes at insert.

To add a stream: implement Collector, register a factory in REGISTRY, enable
it in unjira.config.json.
"""

from __future__ import annotations

from typing import Any, Callable, Iterable, Protocol

from ..events import Event
from ..store import Store


class Collector(Protocol):
    name: str

    def collect(self, store: Store, options: dict[str, Any]) -> Iterable[Event]:
        """Yield new events since this collector's cursor; advance the cursor as you go."""
        ...


def _claude_code() -> Collector:
    from .claude_code import ClaudeCodeCollector

    return ClaudeCodeCollector()


REGISTRY: dict[str, Callable[[], Collector]] = {
    "claude_code": _claude_code,
    # "github": ...   phase 0/1
    # "slack": ...    phase 0/1
    # "jira": ...     phase 1
}
