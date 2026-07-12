from datetime import date, datetime

from unjira.events import Event
from unjira.store import Store


def make_event(external_id: str = "s1:100") -> Event:
    return Event(
        source="claude_code",
        external_id=external_id,
        occurred_at=datetime(2026, 7, 11, 14, 30),
        summary="Claude Code session in unjira: 3 user messages.",
        artifacts={"ticket_keys": ["PROJ-1"]},
    )


def test_insert_dedupes_on_source_and_external_id(tmp_path):
    store = Store(tmp_path / "test.db")
    assert store.insert_event(make_event()) is True
    assert store.insert_event(make_event()) is False
    assert store.insert_event(make_event("s1:200")) is True


def test_events_on_day_filters(tmp_path):
    store = Store(tmp_path / "test.db")
    store.insert_event(make_event())
    assert len(store.events_on(date(2026, 7, 11))) == 1
    assert len(store.events_on(date(2026, 7, 12))) == 0


def test_cursor_roundtrip(tmp_path):
    store = Store(tmp_path / "test.db")
    assert store.get_cursor("claude_code", "/a/b.jsonl") is None
    store.set_cursor("claude_code", "/a/b.jsonl", "123:456")
    assert store.get_cursor("claude_code", "/a/b.jsonl") == "123:456"
    store.set_cursor("claude_code", "/a/b.jsonl", "789:456")
    assert store.get_cursor("claude_code", "/a/b.jsonl") == "789:456"
