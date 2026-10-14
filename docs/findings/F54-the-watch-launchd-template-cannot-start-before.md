# F54 — the watch launchd template cannot start before the operator logs in, or at all headless

`ops/com.unjira.watch.plist` is a per-user `LaunchAgent` (installed under
`~/Library/LaunchAgents`), which launchd only runs inside a logged-in user session. `RunAtLoad` +
`KeepAlive` keeps `watch` running once that session exists and restarts it after a crash, but
neither key makes launchd start it at boot before login, or on a headless/shared Mac where nobody
ever logs in to the GUI session that owns LaunchAgents. A `LaunchDaemon`
(`/Library/LaunchDaemons`, root-owned, runs system-wide independent of any login) would cover
that, but changes the credential story: a daemon runs as a different user by default and the
`internal/envfile` repo-root `.env` convention this template relies on (see the plist's own
comments) assumes `WorkingDirectory` and the operator's checkout are the same user's. **Consequence:**
on a machine that reboots unattended, or where the operator is not reliably logged in (a headless
build box, a shared Mac), `watch` silently stops running until the next login, with nothing short
of checking `launchctl list com.unjira.watch` to notice. Unmeasured — no deployment has hit this
yet; noted while writing the template, not found as a live incident.

## Status

open. A LaunchDaemon would, but changes the credential story; unmeasured
