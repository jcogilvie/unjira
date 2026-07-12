"""Minimal Atlassian Document Format helpers.

Jira Cloud's v3 API requires ADF for rich-text fields (descriptions, comments).
We only need plain paragraphs for now; anything fancier belongs in phase 1's
comment drafting.
"""

from __future__ import annotations

from typing import Any


def text_to_adf(text: str) -> dict[str, Any]:
    paragraphs = [p.strip() for p in text.split("\n\n") if p.strip()]
    return {
        "type": "doc",
        "version": 1,
        "content": [
            {"type": "paragraph", "content": [{"type": "text", "text": p}]}
            for p in (paragraphs or [text])
        ],
    }


def adf_to_text(doc: dict[str, Any] | None) -> str:
    """Best-effort flattening of an ADF document to plain text."""
    if not doc:
        return ""
    parts: list[str] = []

    def walk(node: Any) -> None:
        if isinstance(node, dict):
            if node.get("type") == "text":
                parts.append(node.get("text", ""))
            for child in node.get("content", []) or []:
                walk(child)
            if node.get("type") == "paragraph":
                parts.append("\n")
        elif isinstance(node, list):
            for child in node:
                walk(child)

    walk(doc)
    return "".join(parts).strip()
