"""The collect pass: run every enabled collector, persist new events."""

from __future__ import annotations

from ..collectors import REGISTRY
from ..config import Config
from ..store import Store


def run_collect(config: Config, store: Store) -> dict[str, int]:
    """Returns {collector_name: new_event_count}."""
    results: dict[str, int] = {}
    for name, options in config.enabled_collectors().items():
        factory = REGISTRY.get(name)
        if factory is None:
            results[name] = -1  # enabled in config but not registered
            continue
        collector = factory()
        inserted = 0
        for event in collector.collect(store, options):
            if store.insert_event(event):
                inserted += 1
        results[name] = inserted
    return results
