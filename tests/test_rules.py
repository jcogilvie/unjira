"""Seed rules under rules/ must parse in the documented frontmatter format.

Phase 0 has no rule loader yet (the correlator/reconciler prompt loader is phase 1),
so this test parses the frontmatter inline. It enforces the contract the loader will
rely on, and that the five predecessor-derived norms are actually present.
"""

from __future__ import annotations

from pathlib import Path

import pytest

RULES_DIR = Path(__file__).resolve().parent.parent / "rules"

EXPECTED_RULES = {
    "intent-not-outcome.md",
    "verify-correlations.md",
    "env-mirror-fanout.md",
    "bot-pr-noise.md",
    "review-staleness.md",
}

VALID_SCOPES = {"correlator", "reconciler", "estimator"}
VALID_CONFIDENCE = {"high", "provisional"}


def rule_files() -> list[Path]:
    return [p for p in sorted(RULES_DIR.glob("*.md")) if p.name != "README.md"]


def parse_frontmatter(text: str) -> tuple[dict[str, str], str]:
    """Split a `---`-delimited frontmatter block from the body. Minimal, no YAML dep."""
    if not text.startswith("---"):
        raise ValueError("missing opening frontmatter delimiter")
    _, fm, body = text.split("---", 2)
    meta: dict[str, str] = {}
    for line in fm.strip().splitlines():
        if not line.strip():
            continue
        key, _, value = line.partition(":")
        meta[key.strip()] = value.strip()
    return meta, body.strip()


def test_all_expected_rules_present():
    names = {p.name for p in rule_files()}
    assert EXPECTED_RULES <= names, f"missing: {EXPECTED_RULES - names}"


@pytest.mark.parametrize("path", rule_files(), ids=lambda p: p.name)
def test_rule_has_valid_frontmatter_and_body(path: Path):
    meta, body = parse_frontmatter(path.read_text(encoding="utf-8"))
    for key in ("scope", "confidence", "learned", "source"):
        assert key in meta, f"{path.name} missing frontmatter key: {key}"
    assert meta["scope"] in VALID_SCOPES, f"{path.name} bad scope: {meta['scope']}"
    assert meta["confidence"] in VALID_CONFIDENCE, f"{path.name} bad confidence"
    assert body, f"{path.name} has empty body"
