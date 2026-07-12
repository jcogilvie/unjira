"""Facade tests with a stubbed upstream library.

These cover the logic that is *ours* — pagination loops, shape extraction,
error translation, payload construction. The library's HTTP behavior is
covered by its own test suite; the live tier covers the real contract.
"""

import pytest
from requests import HTTPError

from unjira.jira.client import JiraClient, JiraError


class FakeResponse:
    def __init__(self, status_code: int, body: dict | None = None, text: str = "") -> None:
        self.status_code = status_code
        self._body = body
        self.text = text or str(body)

    def json(self):
        if self._body is None:
            raise ValueError("no json")
        return self._body


class StubUpstream:
    """Only the methods the facade delegates to, returning canned pages."""

    def __init__(self) -> None:
        self.jql_pages: list[dict] = []
        self.changelog_pages: list[dict] = []
        self.posted: list[tuple[str, dict]] = []
        self.jql_calls: list[dict] = []

    def enhanced_jql(self, jql, fields=None, nextPageToken=None, limit=None, expand=None):
        self.jql_calls.append({"token": nextPageToken, "limit": limit})
        return self.jql_pages.pop(0)

    def get_issue_changelog(self, key, start=0, limit=100):
        return self.changelog_pages.pop(0)

    def get_status_for_project(self, key):
        return [
            {"statuses": [{"name": "To Do", "statusCategory": {"key": "new"}}]},
            {"statuses": [{"name": "Done", "statusCategory": {"key": "done"}},
                          {"name": "To Do", "statusCategory": {"key": "new"}}]},
        ]

    def get_issue_transitions_full(self, key):
        return {"transitions": [{"id": "31", "name": "Start", "to": {"name": "In Progress"}}]}

    def resource_url(self, resource):
        return f"rest/api/2/{resource}"

    def post(self, url, data=None):
        self.posted.append((url, data))

    def myself(self):
        raise HTTPError(response=FakeResponse(401, {"errorMessages": ["nope"]}))


def make_client(stub: StubUpstream) -> JiraClient:
    return JiraClient("https://stub", "e", "t", upstream=stub)


def test_error_translated_to_jira_error():
    with pytest.raises(JiraError) as excinfo:
        make_client(StubUpstream()).myself()
    assert excinfo.value.status == 401
    assert "nope" in excinfo.value.message


def test_search_issues_pages_with_next_page_token():
    stub = StubUpstream()
    stub.jql_pages = [
        {"issues": [{"key": "P-1"}, {"key": "P-2"}], "nextPageToken": "t2"},
        {"issues": [{"key": "P-3"}]},
    ]
    keys = [i["key"] for i in make_client(stub).search_issues("project = P")]
    assert keys == ["P-1", "P-2", "P-3"]
    assert stub.jql_calls[1]["token"] == "t2"


def test_search_issues_respects_limit():
    stub = StubUpstream()
    stub.jql_pages = [{"issues": [{"key": "P-1"}, {"key": "P-2"}], "nextPageToken": "more"}]
    keys = [i["key"] for i in make_client(stub).search_issues("project = P", limit=2)]
    assert keys == ["P-1", "P-2"]


def test_changelog_paginates_until_is_last():
    stub = StubUpstream()
    stub.changelog_pages = [
        {"values": [{"items": [{"field": "status", "fromString": "To Do", "toString": "Doing"}]}],
         "isLast": False, "maxResults": 100},
        {"values": [{"items": [
            {"field": "assignee", "fromString": None, "toString": "jon"},
            {"field": "status", "fromString": "Doing", "toString": "Done"},
        ]}], "isLast": True},
    ]
    changes = make_client(stub).status_changes("P-1")
    assert changes == [("To Do", "Doing"), ("Doing", "Done")]


def test_project_statuses_merges_issue_types():
    statuses = make_client(StubUpstream()).project_statuses("P")
    assert statuses == {"To Do": "new", "Done": "done"}


def test_transitions_return_raw_api_shape():
    transitions = make_client(StubUpstream()).get_transitions("P-1")
    assert transitions[0]["to"]["name"] == "In Progress"


def test_transition_with_fields_posts_full_payload():
    stub = StubUpstream()
    make_client(stub).transition_issue("P-1", 31, fields={"resolution": {"name": "Done"}})
    url, payload = stub.posted[0]
    assert url == "rest/api/2/issue/P-1/transitions"
    assert payload == {"transition": {"id": "31"}, "fields": {"resolution": {"name": "Done"}}}
