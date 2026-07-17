"""Env-mirror fan-out clustering (design-notes.md #3).

One logical change ("switch shared-infra to managed mode") fans out into ~12
near-identical per-region PRs. That is one review effort and one narrative — tracking it
as 12 rows is noise and inflates velocity. Cluster same-repo + same-author +
region-normalized-title PRs with consecutive numbers into one range.
"""

from __future__ import annotations

from unjira.correlator.fanout import FanoutItem, cluster_fanout, normalize_title


def item(number: int, title: str, *, repo: str = "infra-repo", author: str = "alice") -> FanoutItem:
    return FanoutItem(repo=repo, author=author, title=title, number=number)


# -- normalize_title --------------------------------------------------------


def test_trailing_paren_region_folds():
    a = normalize_title("Switch shared-infra to managed mode (zrh)")
    b = normalize_title("Switch shared-infra to managed mode (us2)")
    assert a == b


def test_for_connector_region_folds():
    assert normalize_title("Enable managed mode for zrh") == normalize_title(
        "Enable managed mode for us2"
    )


def test_region_substring_in_real_word_is_untouched():
    # "production" contains "prod" but must not fold — word-boundary gated.
    assert "production" in normalize_title("Ship the production release")


# -- cluster_fanout ---------------------------------------------------------


def test_twelve_region_mirrors_collapse_to_one_cluster():
    items = [item(n, f"Switch shared-infra to managed mode ({r})")
             for n, r in zip(range(678, 690),
                             ["zrh", "us2", "tky", "syd", "mon", "kor",
                              "fra", "fed", "dub", "corp", "long", "prod"])]
    clusters = cluster_fanout(items)
    assert len(clusters) == 1
    c = clusters[0]
    assert c.is_fanout
    assert c.span == (678, 689)
    assert c.numbers == tuple(range(678, 690))


def test_non_consecutive_numbers_split_by_gap():
    items = [item(678, "Bump base image for zrh"), item(680, "Bump base image for us2")]
    clusters = cluster_fanout(items)  # default max_gap=1; gap of 2 splits
    assert len(clusters) == 2


def test_different_repo_does_not_merge():
    items = [
        item(1, "Enable managed mode for zrh", repo="repo-a"),
        item(2, "Enable managed mode for us2", repo="repo-b"),
    ]
    assert len(cluster_fanout(items)) == 2


def test_lone_item_is_size_one_non_fanout():
    clusters = cluster_fanout([item(42, "One-off fix for the widget")])
    assert len(clusters) == 1
    assert not clusters[0].is_fanout
    assert clusters[0].numbers == (42,)
