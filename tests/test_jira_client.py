import base64
import json

import httpx
import pytest

from unjira.jira.adf import adf_to_text, text_to_adf
from unjira.jira.client import JiraClient, JiraError


def make_client(handler) -> JiraClient:
    return JiraClient(
        "https://test.atlassian.net",
        "bot@example.com",
        "token123",
        transport=httpx.MockTransport(handler),
    )


def test_sends_basic_auth():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["auth"] = request.headers.get("Authorization")
        return httpx.Response(200, json={"accountId": "abc"})

    make_client(handler).myself()
    expected = base64.b64encode(b"bot@example.com:token123").decode()
    assert seen["auth"] == f"Basic {expected}"


def test_retries_429_then_succeeds():
    calls = {"n": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        if calls["n"] == 1:
            return httpx.Response(429, headers={"Retry-After": "0"})
        return httpx.Response(200, json={"accountId": "abc"})

    assert make_client(handler).myself() == {"accountId": "abc"}
    assert calls["n"] == 2


def test_raises_jira_error_with_message():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(400, json={"errorMessages": ["Field 'foo' is required"]})

    with pytest.raises(JiraError) as excinfo:
        make_client(handler).get_issue("PROJ-1")
    assert excinfo.value.status == 400
    assert "Field 'foo' is required" in excinfo.value.message


def test_search_issues_pages_with_next_page_token():
    def handler(request: httpx.Request) -> httpx.Response:
        token = request.url.params.get("nextPageToken")
        if token is None:
            return httpx.Response(
                200, json={"issues": [{"key": "P-1"}, {"key": "P-2"}], "nextPageToken": "t2"}
            )
        assert token == "t2"
        return httpx.Response(200, json={"issues": [{"key": "P-3"}]})

    keys = [i["key"] for i in make_client(handler).search_issues("project = P")]
    assert keys == ["P-1", "P-2", "P-3"]


def test_search_issues_respects_limit():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200, json={"issues": [{"key": "P-1"}, {"key": "P-2"}], "nextPageToken": "more"}
        )

    keys = [i["key"] for i in make_client(handler).search_issues("project = P", limit=2)]
    assert keys == ["P-1", "P-2"]


def test_transition_payload_shape():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["body"] = json.loads(request.content)
        return httpx.Response(204)

    make_client(handler).transition_issue("P-1", "31", fields={"resolution": {"name": "Done"}})
    assert seen["path"] == "/rest/api/3/issue/P-1/transitions"
    assert seen["body"] == {
        "transition": {"id": "31"},
        "fields": {"resolution": {"name": "Done"}},
    }


def test_status_changes_extracts_only_status_items():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "values": [
                    {
                        "items": [
                            {"field": "status", "fromString": "To Do", "toString": "In Progress"},
                            {"field": "assignee", "fromString": None, "toString": "jon"},
                        ]
                    },
                    {"items": [{"field": "status", "fromString": "In Progress", "toString": "Done"}]},
                ],
                "isLast": True,
            },
        )

    changes = make_client(handler).status_changes("P-1")
    assert changes == [("To Do", "In Progress"), ("In Progress", "Done")]


def test_adf_roundtrip():
    doc = text_to_adf("First paragraph.\n\nSecond paragraph.")
    assert doc["version"] == 1
    assert len(doc["content"]) == 2
    assert adf_to_text(doc) == "First paragraph.\nSecond paragraph."
