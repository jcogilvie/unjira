"""Observed workflow graphs, mined from issue changelogs.

Three-tier workflow knowledge (see README): admin API when available, this
observed graph for planning, and per-issue GET /transitions as ground truth at
execution time. The graph carries edge *frequencies*: the happy path falls out
statistically, and rare edges are a proxy for "unusual move, gate it" — a
sharper rerouting guardrail than status-category direction alone.

Staleness: cache the graph per (project); when the live transitions endpoint
returns an edge the graph doesn't predict, mark it dirty and re-mine.
"""

from __future__ import annotations

import json
from collections import Counter, deque
from pathlib import Path
from typing import Any

from .client import JiraClient


class WorkflowGraph:
    def __init__(self) -> None:
        self.status_categories: dict[str, str] = {}
        self.edges: Counter[tuple[str, str]] = Counter()

    def add_status(self, name: str, category: str = "unknown") -> None:
        self.status_categories.setdefault(name, category)

    def observe(self, from_status: str, to_status: str) -> None:
        self.add_status(from_status)
        self.add_status(to_status)
        self.edges[(from_status, to_status)] += 1

    def neighbors(self, status: str) -> list[str]:
        return [to for (frm, to) in self.edges if frm == status]

    def has_edge(self, from_status: str, to_status: str) -> bool:
        return (from_status, to_status) in self.edges

    def path(self, from_status: str, to_status: str) -> list[str] | None:
        """Shortest observed transition path, inclusive of both endpoints."""
        if from_status == to_status:
            return [from_status]
        queue = deque([[from_status]])
        seen = {from_status}
        while queue:
            trail = queue.popleft()
            for nxt in self.neighbors(trail[-1]):
                if nxt in seen:
                    continue
                if nxt == to_status:
                    return trail + [nxt]
                seen.add(nxt)
                queue.append(trail + [nxt])
        return None

    def rare_edges(self, max_count: int = 1) -> list[tuple[str, str, int]]:
        """Edges taken at most max_count times — candidates for gating."""
        return sorted(
            (frm, to, n) for (frm, to), n in self.edges.items() if n <= max_count
        )

    # -- persistence ---------------------------------------------------------

    def to_dict(self) -> dict[str, Any]:
        return {
            "status_categories": self.status_categories,
            "edges": [{"from": frm, "to": to, "count": n} for (frm, to), n in sorted(self.edges.items())],
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "WorkflowGraph":
        graph = cls()
        graph.status_categories = dict(data.get("status_categories", {}))
        for edge in data.get("edges", []):
            graph.edges[(edge["from"], edge["to"])] = edge["count"]
        return graph

    def save(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(self.to_dict(), indent=2), encoding="utf-8")

    @classmethod
    def load(cls, path: Path) -> "WorkflowGraph":
        return cls.from_dict(json.loads(path.read_text(encoding="utf-8")))


def mine_project(client: JiraClient, project_key: str, max_issues: int = 200) -> WorkflowGraph:
    """Reconstruct the used workflow subgraph from recent issue changelogs.

    The same changelog fetch later feeds estimation calibration — keep the two
    consumers on one code path when that lands.
    """
    graph = WorkflowGraph()
    for name, category in client.project_statuses(project_key).items():
        graph.add_status(name, category)
    jql = f'project = "{project_key}" ORDER BY updated DESC'
    for issue in client.search_issues(jql, fields=["status"], limit=max_issues):
        for from_status, to_status in client.status_changes(issue["key"]):
            graph.observe(from_status, to_status)
    return graph
