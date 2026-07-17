"""Env-mirror fan-out clustering (docs/design-notes.md #3).

Infra work fans out: one logical change ("switch the shared module to managed mode") becomes ~12
near-identical PRs, one per region (`repo#678-689`). Reviewing all 12 is *one* effort, not 12 —
and counting them as 12 rows inflates velocity and buries the single decision.

`normalize_title` folds region tokens so mirror titles compare equal; `cluster_fanout` groups
by (repo, author, normalized-title) and then splits each group into runs of consecutive PR
numbers. Same author + same normalized title + adjacent numbers ⇒ one narrative with a range.

Deliberately deterministic and evidence-only: it decides nothing about *meaning* (that is the
LLM correlator's job) — it just collapses the mechanical fan-out so the correlator sees one
story instead of twelve fragments.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

# Known deployment regions/environments that mirror PRs fan out across.
REGIONS = (
    "zrh", "us2", "tky", "syd", "mon", "kor",
    "fra", "fed", "dub", "corp", "long", "prod", "dev", "stag",
)

_REGION = "|".join(REGIONS)
# Trailing "(region)" qualifier, e.g. "... managed mode (zrh)".
_TRAILING_PAREN = re.compile(rf"\s*\((?:{_REGION})\)\s*$", re.IGNORECASE)
# Connector + region, e.g. "for zrh" / "in us2" / "switch tky". Keep the connector, drop region.
_CONNECTOR_REGION = re.compile(rf"\b(for|in|switch)\s+(?:{_REGION})\b", re.IGNORECASE)
# "-region" suffix token, e.g. "bump-base-zrh". Word-boundary gated so "-production" is safe.
_SUFFIX_REGION = re.compile(rf"-(?:{_REGION})\b", re.IGNORECASE)
_WHITESPACE = re.compile(r"\s+")


def normalize_title(title: str) -> str:
    """Fold region tokens out of a title so env-mirror variants compare equal.

    Word-boundary gated: `production` (which contains `prod`) is left intact. Case- and
    whitespace-insensitive; returns a lowercased, whitespace-collapsed key.
    """
    text = _TRAILING_PAREN.sub("", title)
    text = _CONNECTOR_REGION.sub(r"\1", text)
    text = _SUFFIX_REGION.sub("", text)
    return _WHITESPACE.sub(" ", text).strip().lower()


@dataclass(frozen=True)
class FanoutItem:
    """One reviewable unit (a PR) considered for fan-out clustering."""

    repo: str
    author: str
    title: str
    number: int


@dataclass(frozen=True)
class FanoutCluster:
    """A run of same-repo/author/normalized-title items with consecutive numbers."""

    repo: str
    author: str
    normalized_title: str
    numbers: tuple[int, ...]

    @property
    def span(self) -> tuple[int, int]:
        return (self.numbers[0], self.numbers[-1])

    @property
    def is_fanout(self) -> bool:
        return len(self.numbers) > 1


def cluster_fanout(items: list[FanoutItem], *, max_gap: int = 1) -> list[FanoutCluster]:
    """Collapse env-mirror fan-out into clusters.

    Items sharing (repo, author, normalized-title) are grouped; each group is split into runs
    where consecutive numbers differ by at most `max_gap`. Singletons come back as size-1,
    non-fanout clusters. Groups are returned in first-seen order; runs within a group ascend.
    """
    groups: dict[tuple[str, str, str], list[FanoutItem]] = {}
    for it in items:
        key = (it.repo, it.author, normalize_title(it.title))
        groups.setdefault(key, []).append(it)

    clusters: list[FanoutCluster] = []
    for (repo, author, norm), members in groups.items():
        numbers = sorted({m.number for m in members})
        run: list[int] = [numbers[0]]
        for n in numbers[1:]:
            if n - run[-1] <= max_gap:
                run.append(n)
            else:
                clusters.append(FanoutCluster(repo, author, norm, tuple(run)))
                run = [n]
        clusters.append(FanoutCluster(repo, author, norm, tuple(run)))
    return clusters
