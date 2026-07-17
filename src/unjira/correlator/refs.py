"""Fully-qualified, range-aware PR/issue references (see docs/design-notes.md #4).

Two predecessor bugs motivate this module:

- **Bare-number ambiguity.** Once the scan went multi-org, `#382` was ambiguous
  (`repo-a#382` != `org/repo-b#382`). A dedup key on the bare number silently merged
  distinct PRs. Fix: key on the full `owner/repo#N`.
- **Range collapse.** Ledgers collapse fan-out families into ranges (`#655-665`). A dedup
  set built from the literal text saw only the endpoints and re-added the interior members
  as "new". Fix: expand ranges into members *before* deduping.

The reference is written `owner/repo#N`, `repo#N`, or bare `#N`; a bare ref only becomes a
Ref when a `default_repo` is supplied (otherwise it is too ambiguous to key on and is
skipped). Ranges `#A-B` (hyphen or en-dash) expand to inclusive members.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

# repo (optional, directly abutting '#'), a start number, and an optional -end range.
# The repo group cannot span whitespace, so a bare "#382" (space before '#') leaves it None.
_REF_RE = re.compile(r"(?P<repo>[A-Za-z0-9._/-]+)?#(?P<start>\d+)(?:[-–](?P<end>\d+))?")

DEFAULT_MAX_SPAN = 500


@dataclass(frozen=True)
class Ref:
    """A single fully-qualified PR/issue reference. `repo` is as-written (may lack an owner)."""

    repo: str
    number: int

    @property
    def key(self) -> str:
        return f"{self.repo}#{self.number}"


def parse_pr_refs(
    text: str, *, default_repo: str | None = None, max_span: int = DEFAULT_MAX_SPAN
) -> list[Ref]:
    """Parse refs from text, expanding ranges to members, order-preserving and deduplicated.

    Args:
        text: free text possibly containing `owner/repo#N`, `repo#N`, `#N`, or `#A-B` ranges.
        default_repo: repo to attach to bare `#N` refs; bare refs are skipped when None.
        max_span: maximum members a single range may expand to. A larger range raises
            ValueError rather than silently truncating (design-notes.md #9: error loudly).

    Returns:
        Refs in first-seen order with duplicates (including range overlaps) removed.
    """
    seen: set[Ref] = set()
    out: list[Ref] = []

    for match in _REF_RE.finditer(text):
        repo = match.group("repo") or default_repo
        if not repo:
            continue  # bare ref with no default: too ambiguous to key on
        start = int(match.group("start"))
        end_raw = match.group("end")

        if end_raw is None or int(end_raw) < start:
            # single ref, or an inverted "range" we decline to interpret as a range
            numbers = [start]
        else:
            end = int(end_raw)
            span = end - start + 1
            if span > max_span:
                raise ValueError(
                    f"range {repo}#{start}-{end} spans {span} members (> max_span={max_span}); "
                    "refusing to expand silently"
                )
            numbers = list(range(start, end + 1))

        for number in numbers:
            ref = Ref(repo, number)
            if ref not in seen:
                seen.add(ref)
                out.append(ref)

    return out
