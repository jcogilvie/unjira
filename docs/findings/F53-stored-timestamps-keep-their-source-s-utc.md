# F53 — stored timestamps keep their source's UTC offset, and window queries compare them as strings

`store.InsertEvent` writes `event.OccurredAt.Format(time.RFC3339)` (`internal/store/events.go:29`)
without converting to UTC, so a timestamp keeps the offset its source gave it. Jira's carry the
account's zone (`2026-08-21T12:00:57.812-0700`, the layout the Jira collector parses), and are stored as
`…-07:00`. Claude Code and GitHub timestamps are `Z`. The window queries then compare these as text:
event-range reads bound `occurred_at` with `start.Format(time.RFC3339)`, and `NarrativesOverlapping`
tests narrative windows the same way. Lexical order equals chronological order only within a single
offset. **Consequence:** a Jira event can fall on the wrong side of a pass's window, or be missed by an
overlap test, by up to its offset (7 hours for that account). The F46 fix compares instants
(`strftime('%s')`) where it decides "forward", and leaves the existing comparisons as they were.
Unmeasured: every event in the snapshots measured in this period is `Z`, since those windows held no
Jira events. The likely fix is to store UTC at every writer, which needs a fresh store under the
no-migrations rule, or to compare instants in the queries.

## Status

open. Jira timestamps are stored with the account's offset, everything else `Z`; unmeasured
