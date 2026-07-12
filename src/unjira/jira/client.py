"""Thin typed client for the Jira Cloud REST API (v3).

Design rules:
- Read methods are safe everywhere. Write methods (create/transition/comment/
  delete) exist as client capability; *authorization* to call them lives in the
  pipeline (review queue, autonomy graduation), never here.
- Per-issue GET /transitions is the ground truth for legal moves at execution
  time; the mined WorkflowGraph (workflow.py) is only for planning.
- Retries 429/503 with Retry-After. Everything else raises JiraError.
"""

from __future__ import annotations

import os
import time
from typing import Any, Iterator

import httpx

from .adf import text_to_adf
from ..config import Config

RETRYABLE = {429, 503}
MAX_RETRIES = 3
SEED_LABEL = "unjira-seed"


class JiraError(Exception):
    def __init__(self, status: int, message: str) -> None:
        super().__init__(f"Jira API {status}: {message}")
        self.status = status
        self.message = message


class JiraClient:
    def __init__(self, site: str, email: str, token: str, transport: httpx.BaseTransport | None = None) -> None:
        if not site:
            raise ValueError("Jira site URL is required (config jira.site or UNJIRA_JIRA_SITE)")
        self._http = httpx.Client(
            base_url=site.rstrip("/"),
            auth=(email, token),
            headers={"Accept": "application/json"},
            timeout=30.0,
            transport=transport,
        )

    @classmethod
    def from_config(cls, config: Config) -> "JiraClient":
        site = os.environ.get("UNJIRA_JIRA_SITE") or config.jira.site
        email = os.environ.get("UNJIRA_JIRA_EMAIL", "")
        token = os.environ.get("UNJIRA_JIRA_TOKEN", "")
        if not email or not token:
            raise ValueError("Set UNJIRA_JIRA_EMAIL and UNJIRA_JIRA_TOKEN (see .env.example)")
        return cls(site, email, token)

    def close(self) -> None:
        self._http.close()

    def _request(self, method: str, path: str, **kwargs: Any) -> httpx.Response:
        for attempt in range(MAX_RETRIES + 1):
            response = self._http.request(method, path, **kwargs)
            if response.status_code in RETRYABLE and attempt < MAX_RETRIES:
                time.sleep(float(response.headers.get("Retry-After", "1")))
                continue
            if response.status_code >= 400:
                raise JiraError(response.status_code, _error_message(response))
            return response
        raise JiraError(response.status_code, _error_message(response))

    def _get(self, path: str, params: dict[str, Any] | None = None) -> Any:
        return self._request("GET", path, params=params).json()

    # -- reads ---------------------------------------------------------------

    def myself(self) -> dict[str, Any]:
        return self._get("/rest/api/3/myself")

    def search_projects(self) -> list[dict[str, Any]]:
        projects: list[dict[str, Any]] = []
        start = 0
        while True:
            page = self._get("/rest/api/3/project/search", params={"startAt": start, "maxResults": 50})
            projects.extend(page.get("values", []))
            if page.get("isLast", True):
                return projects
            start += page.get("maxResults", 50)

    def project_statuses(self, project_key: str) -> dict[str, str]:
        """Status name -> status category key, across all issue types in the project."""
        statuses: dict[str, str] = {}
        for issue_type in self._get(f"/rest/api/3/project/{project_key}/statuses"):
            for status in issue_type.get("statuses", []):
                category = (status.get("statusCategory") or {}).get("key", "unknown")
                statuses[status["name"]] = category
        return statuses

    def search_issues(
        self, jql: str, fields: list[str] | None = None, limit: int = 200
    ) -> Iterator[dict[str, Any]]:
        """Paginated issue search via /search/jql (the old /search was retired in 2025)."""
        params: dict[str, Any] = {"jql": jql, "maxResults": min(limit, 100)}
        if fields:
            params["fields"] = ",".join(fields)
        yielded = 0
        while True:
            page = self._get("/rest/api/3/search/jql", params=params)
            for issue in page.get("issues", []):
                yield issue
                yielded += 1
                if yielded >= limit:
                    return
            token = page.get("nextPageToken")
            if not token:
                return
            params["nextPageToken"] = token

    def get_issue(self, key: str, expand: str | None = None) -> dict[str, Any]:
        params = {"expand": expand} if expand else None
        return self._get(f"/rest/api/3/issue/{key}", params=params)

    def get_changelog(self, key: str) -> list[dict[str, Any]]:
        entries: list[dict[str, Any]] = []
        start = 0
        while True:
            page = self._get(
                f"/rest/api/3/issue/{key}/changelog", params={"startAt": start, "maxResults": 100}
            )
            entries.extend(page.get("values", []))
            if page.get("isLast", True):
                return entries
            start += page.get("maxResults", 100)

    def status_changes(self, key: str) -> list[tuple[str, str]]:
        """(from_status, to_status) pairs from an issue's changelog, oldest first."""
        changes: list[tuple[str, str]] = []
        for entry in self.get_changelog(key):
            for item in entry.get("items", []):
                if item.get("field") == "status":
                    changes.append((item.get("fromString") or "?", item.get("toString") or "?"))
        return changes

    def get_transitions(self, key: str) -> list[dict[str, Any]]:
        return self._get(f"/rest/api/3/issue/{key}/transitions").get("transitions", [])

    # -- writes (pipeline gates authorization; see module docstring) ----------

    def create_issue(
        self,
        project_key: str,
        summary: str,
        issue_type: str = "Task",
        description: str | None = None,
        labels: list[str] | None = None,
    ) -> dict[str, Any]:
        fields: dict[str, Any] = {
            "project": {"key": project_key},
            "summary": summary,
            "issuetype": {"name": issue_type},
        }
        if description:
            fields["description"] = text_to_adf(description)
        if labels:
            fields["labels"] = labels
        return self._request("POST", "/rest/api/3/issue", json={"fields": fields}).json()

    def transition_issue(
        self, key: str, transition_id: str, fields: dict[str, Any] | None = None
    ) -> None:
        payload: dict[str, Any] = {"transition": {"id": str(transition_id)}}
        if fields:
            payload["fields"] = fields
        self._request("POST", f"/rest/api/3/issue/{key}/transitions", json=payload)

    def add_comment(self, key: str, text: str) -> dict[str, Any]:
        return self._request(
            "POST", f"/rest/api/3/issue/{key}/comment", json={"body": text_to_adf(text)}
        ).json()

    def delete_issue(self, key: str) -> None:
        self._request("DELETE", f"/rest/api/3/issue/{key}")


def _error_message(response: httpx.Response) -> str:
    try:
        body = response.json()
    except ValueError:
        return response.text[:200]
    messages = body.get("errorMessages") or []
    messages += [f"{k}: {v}" for k, v in (body.get("errors") or {}).items()]
    return "; ".join(messages) or response.text[:200]
