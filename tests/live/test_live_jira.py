"""Live integration tests against the dev Jira instance.

Gated twice: the `live` marker keeps them out of default runs, and the
UNJIRA_LIVE=1 check makes the intent explicit — these tests WRITE to the
instance (and clean up after themselves). Locally: `UNJIRA_LIVE=1 pytest -m live`.
In CI: the manually-triggered integration job.
"""

import os

import pytest

from unjira.jira.client import SEED_LABEL, JiraClient
from unjira.jira.workflow import mine_project

pytestmark = [
    pytest.mark.live,
    pytest.mark.skipif(os.environ.get("UNJIRA_LIVE") != "1", reason="set UNJIRA_LIVE=1 to run"),
]

SITE = os.environ.get("UNJIRA_JIRA_SITE", "https://unjira.atlassian.net")
PROJECT = os.environ.get("UNJIRA_LIVE_PROJECT", "SCRUM")


@pytest.fixture(scope="module")
def client():
    email = os.environ.get("UNJIRA_JIRA_EMAIL")
    token = os.environ.get("UNJIRA_JIRA_TOKEN")
    if not email or not token:
        pytest.skip("UNJIRA_JIRA_EMAIL / UNJIRA_JIRA_TOKEN not set")
    client = JiraClient(SITE, email, token)
    yield client
    client.close()


def test_auth_and_project_visible(client):
    assert client.myself().get("accountId")
    assert PROJECT in [p["key"] for p in client.search_projects()]


def test_issue_lifecycle_roundtrip(client):
    """Create -> transition -> comment -> changelog -> delete, asserting each hop."""
    issue = client.create_issue(
        PROJECT,
        summary="[seed] live roundtrip test issue",
        description="Created by tests/live; deleted by this test's cleanup.",
        labels=[SEED_LABEL],
    )
    key = issue["key"]
    try:
        transitions = client.get_transitions(key)
        assert transitions, "expected at least one legal transition from the initial status"
        target = transitions[0]
        client.transition_issue(key, target["id"])

        refreshed = client.get_issue(key, expand=None)
        assert refreshed["fields"]["status"]["name"] == target["to"]["name"]

        client.add_comment(key, "Live-test comment.\n\nSecond paragraph survives the trip.")

        changes = client.status_changes(key)
        assert any(to == target["to"]["name"] for _, to in changes)
    finally:
        client.delete_issue(key)


def test_workflow_mining_produces_a_graph(client):
    graph = mine_project(client, PROJECT, max_issues=50)
    assert graph.status_categories, "project should report at least one status"
    # Edges may be empty on a virgin instance; statuses must still carry categories.
    assert all(isinstance(category, str) for category in graph.status_categories.values())
