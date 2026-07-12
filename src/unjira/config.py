"""Configuration loading. Copy config/unjira.example.json to ./unjira.config.json."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from pydantic import BaseModel, Field

DEFAULT_CONFIG_PATH = Path("unjira.config.json")


class JiraConfig(BaseModel):
    site: str = ""
    project_keys: list[str] = Field(default_factory=list)
    # Credentials come from env (JIRA_BOT_EMAIL / JIRA_BOT_API_TOKEN), never from this file.


class Config(BaseModel):
    jira: JiraConfig = Field(default_factory=JiraConfig)
    collectors: dict[str, dict[str, Any]] = Field(
        default_factory=lambda: {"claude_code": {"enabled": True}}
    )
    db_path: Path = Path("data/unjira.db")

    def enabled_collectors(self) -> dict[str, dict[str, Any]]:
        return {name: opts for name, opts in self.collectors.items() if opts.get("enabled")}


def load_config(path: Path | None = None) -> Config:
    path = path or DEFAULT_CONFIG_PATH
    if path.exists():
        return Config.model_validate(json.loads(path.read_text(encoding="utf-8")))
    return Config()
