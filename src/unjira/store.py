"""SQLite persistence: events, collector cursors, and the phase-1+ tables.

The event log is append-only; (source, external_id) is the dedup key so
collectors can safely re-emit. Narratives/actions/estimates/ledger are created
now so the schema is stable, but only events and cursors are written in phase 0.
"""

from __future__ import annotations

import json
import sqlite3
from datetime import date, datetime, timedelta
from pathlib import Path

from .events import Event

SCHEMA = """
CREATE TABLE IF NOT EXISTS events (
    id           INTEGER PRIMARY KEY,
    source       TEXT NOT NULL,
    external_id  TEXT NOT NULL,
    occurred_at  TEXT NOT NULL,
    actor        TEXT,
    summary      TEXT NOT NULL,
    artifacts    TEXT NOT NULL DEFAULT '{}',
    raw_ref      TEXT,
    ingested_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (source, external_id)
);
CREATE INDEX IF NOT EXISTS idx_events_occurred ON events (occurred_at);

CREATE TABLE IF NOT EXISTS cursors (
    collector  TEXT NOT NULL,
    resource   TEXT NOT NULL,
    position   TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (collector, resource)
);

CREATE TABLE IF NOT EXISTS narratives (
    id           INTEGER PRIMARY KEY,
    window_start TEXT NOT NULL,
    window_end   TEXT NOT NULL,
    title        TEXT NOT NULL,
    summary      TEXT NOT NULL,
    issue_key    TEXT,
    confidence   REAL,
    status       TEXT NOT NULL DEFAULT 'open',
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS narrative_events (
    narrative_id INTEGER NOT NULL REFERENCES narratives (id),
    event_id     INTEGER NOT NULL REFERENCES events (id),
    PRIMARY KEY (narrative_id, event_id)
);

CREATE TABLE IF NOT EXISTS actions (
    id           INTEGER PRIMARY KEY,
    narrative_id INTEGER REFERENCES narratives (id),
    type         TEXT NOT NULL,       -- comment | transition | create | estimate
    issue_key    TEXT,
    payload      TEXT NOT NULL,       -- JSON, shape depends on type
    confidence   REAL,
    rationale    TEXT,
    status       TEXT NOT NULL DEFAULT 'proposed',
                                      -- proposed | approved | edited | rejected | executed | failed
    decided_at   TEXT,
    executed_at  TEXT,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS estimates (
    id         INTEGER PRIMARY KEY,
    issue_key  TEXT NOT NULL,
    method     TEXT NOT NULL,         -- which framing produced it, or 'ensemble'
    value      REAL NOT NULL,
    spread     REAL,
    actual     REAL,                  -- backfilled after completion, for calibration
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS ledger (
    id              INTEGER PRIMARY KEY,
    occurred_at     TEXT NOT NULL,
    description     TEXT NOT NULL,
    source_event_id INTEGER REFERENCES events (id),
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
"""


class Store:
    def __init__(self, db_path: Path) -> None:
        db_path.parent.mkdir(parents=True, exist_ok=True)
        self.conn = sqlite3.connect(db_path)
        self.conn.row_factory = sqlite3.Row
        self.conn.executescript(SCHEMA)

    def close(self) -> None:
        self.conn.close()

    # -- events ------------------------------------------------------------

    def insert_event(self, event: Event) -> bool:
        """Insert an event; returns False if it was already present."""
        cur = self.conn.execute(
            """
            INSERT OR IGNORE INTO events (source, external_id, occurred_at, actor, summary, artifacts, raw_ref)
            VALUES (?, ?, ?, ?, ?, ?, ?)
            """,
            (
                event.source,
                event.external_id,
                event.occurred_at.isoformat(),
                event.actor,
                event.summary,
                json.dumps(event.artifacts, default=str),
                event.raw_ref,
            ),
        )
        self.conn.commit()
        return cur.rowcount > 0

    def events_on(self, day: date) -> list[sqlite3.Row]:
        start = datetime.combine(day, datetime.min.time())
        end = start + timedelta(days=1)
        return self.conn.execute(
            "SELECT * FROM events WHERE occurred_at >= ? AND occurred_at < ? ORDER BY occurred_at",
            (start.isoformat(), end.isoformat()),
        ).fetchall()

    def event_counts_by_source(self) -> list[sqlite3.Row]:
        return self.conn.execute(
            "SELECT source, COUNT(*) AS n, MAX(occurred_at) AS latest FROM events GROUP BY source"
        ).fetchall()

    # -- cursors -----------------------------------------------------------

    def get_cursor(self, collector: str, resource: str) -> str | None:
        row = self.conn.execute(
            "SELECT position FROM cursors WHERE collector = ? AND resource = ?",
            (collector, resource),
        ).fetchone()
        return row["position"] if row else None

    def set_cursor(self, collector: str, resource: str, position: str) -> None:
        self.conn.execute(
            """
            INSERT INTO cursors (collector, resource, position, updated_at)
            VALUES (?, ?, ?, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
            ON CONFLICT (collector, resource)
            DO UPDATE SET position = excluded.position, updated_at = excluded.updated_at
            """,
            (collector, resource, position),
        )
        self.conn.commit()

    def cursor_counts(self) -> list[sqlite3.Row]:
        return self.conn.execute(
            "SELECT collector, COUNT(*) AS n, MAX(updated_at) AS latest FROM cursors GROUP BY collector"
        ).fetchall()
