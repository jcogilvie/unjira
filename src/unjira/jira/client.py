"""Jira access: a thin facade over atlassian-python-api.

The facade is the seam. Everything above it (collectors, workflow mining,
devtools, the future executor) sees this stable, minimal surface; the
community library underneath absorbs Jira Cloud API churn — it shipped the
/search -> /search/jql migration for everyone, which is exactly the shared
maintenance we want to ride. If upstream ever rots, reimplementing this
surface directly is deliberately small.

Design rules that survive the library swap:
- Read methods are safe everywhere. Write methods exist as client capability;
  *authorization* to call them lives in the pipeline (review queue, autonomy
  graduation), never here.
- Per-issue transitions are the ground truth for legal moves at execution
  time; the mined WorkflowGraph (workflow.py) is only for planning.
- Retries on 413/429/503 honor Retry-After (library backoff_and_retry).
"""

from __future__ import annotations

import os
from typing import Any, Iterator

from atlassian import Jira as UpstreamJira
from requests import HTTPError

from ..config import Config

SEED_LABEL = "unjira-seed"


class JiraError(Exception):
    def __init__(self, status: int, message: str) -> None:
        super().__init__(f"Jira API {status}: {message}")
        self.status = status
        self.message = message


class JiraClient:
    def __init__(self, site: str, email: str, token: str, upstream: Any | None = None) -> None:
        if not site and upstream is None:
            raise ValueError("Jira site URL is required (config jira.site or UNJIRA_JIRA_SITE)")
        self._jira = upstream or UpstreamJira(
            url=site,
            username=email,
            password=token,
            cloud=True,
            timeout=30,
            backoff_and_retry=True,
            retry_with_header=True,
            max_backoff_retries=5,
            max_backoff_seconds=60,
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
        session = getattr(self._jira, "_session", None)
        if session is not None:
            session.close()

    def _call(self, method: Any, *args: Any, **kwargs: Any) -> Any:
        try:
            return method(*args, **kwargs)
        except HTTPError as exc:
            response = exc.response
            status = response.status_code if response is not None else 0
            raise JiraError(status, _error_message(response)) from exc

    # -- reads ---------------------------------------------------------------

    def myself(self) -> dict[str, Any]:
        return self._call(self._jira.myself)

    def search_projects(self) -> list[dict[str, Any]]:
        return list(self._call(self._jira.projects) or [])

    def project_statuses(self, project_key: str) -> dict[str, str]:
        """Status name -> status category key, across all issue types in the project."""
        statuses: dict[str, str] = {}
        for issue_type in self._call(self._jira.get_status_for_project, project_key) or []:
            for status in issue_type.get("statuses", []):
                category = (status.get("statusCategory") or {}).get("key", "unknown")
                statuses[status["name"]] = category
        return statuses

    def search_issues(
        self, jql: str, fields: list[str] | None = None, limit: int = 200
    ) -> Iterator[dict[str, Any]]:
        """Paginated issue search via /search/jql (upstream's enhanced_jql)."""
        field_param = ",".join(fields) if fields else "*all"
        token: str | None = None
        yielded = 0
        while True:
            page = self._call(
                self._jira.enhanced_jql,
                jql,
                fields=field_param,
                nextPageToken=token,
                limit=min(limit - yielded, 100),
            )
            for issue in page.get("issues", []):
                yield issue
                yielded += 1
                if yielded >= limit:
                    return
            token = page.get("nextPageToken")
            if not token:
                return

    def get_issue(self, key: str, expand: str | None = None) -> dict[str, Any]:
        return self._call(self._jira.get_issue, key, expand=expand)

    def get_changelog(self, key: str) -> list[dict[str, Any]]:
        entries: list[dict[str, Any]] = []
        start = 0
        while True:
            page = self._call(self._jira.get_issue_changelog, key, start=start, limit=100)
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
        """Raw transitions (API shape: id, name, to.name, to.statusCategory, ...)."""
        raw = self._call(self._jira.get_issue_transitions_full, key) or {}
        return raw.get("transitions", [])

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
            fields["description"] = description
        if labels:
            fields["labels"] = labels
        return self._call(self._jira.issue_create, fields)

    def transition_issue(
        self, key: str, transition_id: str, fields: dict[str, Any] | None = None
    ) -> None:
        payload: dict[str, Any] = {"transition": {"id": str(transition_id)}}
        if fields:
            payload["fields"] = fields
        url = f"{self._jira.resource_url('issue')}/{key}/transitions"
        self._call(self._jira.post, url, data=payload)

    def add_comment(self, key: str, text: str) -> dict[str, Any]:
        return self._call(self._jira.issue_add_comment, key, text)

    def delete_issue(self, key: str) -> None:
        self._call(self._jira.delete_issue, key)


def _error_message(response: Any) -> str:
    if response is None:
        return "no response"
    try:
        body = response.json()
    except ValueError:
        return response.text[:200]
    messages = list(body.get("errorMessages") or [])
    messages += [f"{k}: {v}" for k, v in (body.get("errors") or {}).items()]
    return "; ".join(messages) or response.text[:200]
