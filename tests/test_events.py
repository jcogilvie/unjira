from unjira.events import extract_ticket_keys, is_sentinel_key


def test_extracts_and_dedupes_in_order():
    text = "PROJ-123 fixes AUTH-9; see PROJ-123 and proj-99 (lowercase ignored)"
    assert extract_ticket_keys(text) == ["PROJ-123", "AUTH-9"]


def test_sentinel_detection_is_prefix_agnostic():
    assert is_sentinel_key("XYZ-0")
    assert is_sentinel_key("PROJ-0")
    assert is_sentinel_key("LONGERKEY-1")
    assert not is_sentinel_key("XYZ-10")
    assert not is_sentinel_key("PROJ-123")
