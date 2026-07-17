"""Fully-qualified, range-aware PR/issue reference parsing (design-notes.md #4).

Two real predecessor bugs this guards against:
- bare `#382` collided across orgs (repo-a#382 vs org/repo-b#382);
- ledger ranges like `#655-665` were deduped on literal text, so interior members
  re-appeared as "new". Keys must be fully qualified; ranges must expand to members.
"""

from __future__ import annotations

import pytest

from unjira.correlator.refs import Ref, parse_pr_refs


def keys(text: str, **kw) -> list[str]:
    return [r.key for r in parse_pr_refs(text, **kw)]


def test_full_ref_no_bare_number_collision():
    ks = keys("see repo-a#382 and org/repo-b#382")
    assert ks == ["repo-a#382", "org/repo-b#382"]
    assert len(set(ks)) == 2


def test_hyphen_range_expands_to_members():
    assert keys("infra-repo#678-689") == [f"infra-repo#{n}" for n in range(678, 690)]


def test_endash_range_expands_inclusive():
    # 655..665 inclusive = 11 members, endpoints and interior
    ks = keys("fan-out infra-repo#655–665 landed")
    assert ks == [f"infra-repo#{n}" for n in range(655, 666)]
    assert len(ks) == 11


def test_bare_ref_uses_default_repo_or_is_skipped():
    assert keys("bumped #382", default_repo="o/r") == ["o/r#382"]
    assert keys("bumped #382") == []


def test_oversized_range_errors_loudly():
    with pytest.raises(ValueError):
        parse_pr_refs("repo#1-100000", max_span=500)


def test_order_preserving_and_deduplicated():
    text = "repo#5 repo#5 repo#3-4 repo#4"
    assert keys(text) == ["repo#5", "repo#3", "repo#4"]


def test_inverted_range_is_single_ref():
    assert keys("repo#665-655") == ["repo#665"]


def test_ref_is_hashable_value_type():
    assert Ref("o/r", 1) == Ref("o/r", 1)
    assert len({Ref("o/r", 1), Ref("o/r", 1)}) == 1
    assert Ref("o/r", 1).key == "o/r#1"
