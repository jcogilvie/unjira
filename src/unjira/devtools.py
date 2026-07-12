"""Seed and reset the dev Jira instance with realistic, labeled test data.

Everything created here carries the unjira-seed label so reset can find and
remove exactly what seed created, nothing else. Transitioning seeded issues
along legal paths generates changelog history — the raw material for workflow
mining and, later, estimation calibration. Doubles as the demo-environment
builder.
"""

from __future__ import annotations

import random

from .jira.client import SEED_LABEL, JiraClient

SEED_SUMMARIES = [
    "Increase resource requests for ingress controller",
    "Fix retry logic in payment client",
    "Add structured logging to auth service",
    "Investigate flaky checkout integration test",
    "Upgrade postgres client library",
    "Document rollback procedure for deploys",
    "Rate-limit the webhook receiver",
    "Migrate cron jobs to the new scheduler",
]


def seed(client: JiraClient, project_key: str, count: int = 6, rng: random.Random | None = None) -> list[str]:
    """Create labeled issues and walk them along legal transitions. Returns keys."""
    rng = rng or random.Random(11)
    keys: list[str] = []
    for i in range(count):
        summary = SEED_SUMMARIES[i % len(SEED_SUMMARIES)]
        issue = client.create_issue(
            project_key,
            summary=f"[seed] {summary}",
            description=f"Seeded by unjira devtools (item {i + 1} of {count}).",
            labels=[SEED_LABEL],
        )
        key = issue["key"]
        keys.append(key)
        for _ in range(rng.randint(0, 3)):
            transitions = client.get_transitions(key)
            if not transitions:
                break
            choice = rng.choice(transitions)
            client.transition_issue(key, choice["id"])
        if rng.random() < 0.5:
            client.add_comment(key, "Seeded comment: investigation notes go here.")
    return keys


def reset(client: JiraClient, project_key: str) -> list[str]:
    """Delete every seed-labeled issue in the project. Returns deleted keys."""
    jql = f'project = "{project_key}" AND labels = "{SEED_LABEL}"'
    keys = [issue["key"] for issue in client.search_issues(jql, fields=["status"], limit=500)]
    for key in keys:
        client.delete_issue(key)
    return keys
