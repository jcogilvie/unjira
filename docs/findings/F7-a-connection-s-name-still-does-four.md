# F7 — a connection's name still does four jobs

**Narrowed by the tracker model's slice 1** (`docs/superpowers/specs/2026-10-06-tracker-model-design.md`).
The one struct that held endpoint, identity, read scope, write scope and collection config is now two:
`config.Connection` (`internal/config/trackers.go`) holds kind and endpoint, and `config.Tracker` holds
scopes, `writable_scopes`, `default_scope`, `mirror_to` and the Jira `queries`. Write-scope separation
survives unchanged — `writable_scopes` does not default to `scopes`.

**What remains:** identity still has no field of its own. `Connection.Name` is the config key, the
`UNJIRA_JIRA_CREDENTIALS` lookup key, the Jira collector's cursor prefix
(`collector/jira/cursor.go:24`), and the connection recorded on every stored link
(`narrative_issues.connection`). Renaming a connection therefore resets its cursors, and leaves its
old links naming a connection that no longer exists. Verification no longer reads that column — a key
routes by its scope — so the stale name is provenance only, but it is still the one place a link
records where it came from. **#178**'s kubeconfig-like question (a user
by name, separate from the server) is still open on its own terms.

## Status

**#178**, narrowed: connection and tracker are split (tracker model slice 1); a connection's name still carries identity, credential lookup, cursor prefix and stored-link provenance
