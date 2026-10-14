# F48 — a script handed to a shell is read as data, so the commands it runs are invisible

`simpleCommands` (`internal/collector/claudecode/shell.go`) parses a tool call's command with
mvdan.cc/sh and counts only the simple commands it runs directly. A script passed to another shell is a
string or a heredoc body to the parser, so its commands never reach `isAuthoring`, the fact rules or
the `gh pr create` anchor recognizer. That covers `sh -c '…'`, `bash <<'EOF'`, and `docker run … sh -c
'…'`. A Python heredoc that shells out (`subprocess.run(["git", "commit", …])`) is invisible the same
way. **Consequence:** a commit or PR made that way loses its SCM keys and facts, and a PR created that
way gets no anchor.

Measured over every local transcript, by ISO week. Heredocs stepped from 1–9% of distinct commands
(W18–W33) to 11–16% (W34–W40), and Python heredocs from about 10 a week to 130–580. Through both
periods, none of the scripts handed to a shell (95) and none of the Python heredocs (2,082) that shell
out named a commit or PR verb. Since W34, with 0 of ~1,890 Python heredocs and 0 of ~47 shell scripts,
the rule-of-three upper bound is under one hidden commit or PR a week. Visible authoring runs at 70–190
commands a week. The one real case seen is a test run, `helm unittest` inside `docker run … sh -c`,
which costs a "ran tests" fact.

The measurement extrapolates a volume, not a behaviour: if commits start being made from inside
scripts, it moves within a week. `TestHiddenAuthoring_Tripwire`
(`internal/collector/claudecode/hidden_authoring_probe_test.go`) is the guard: it re-measures by ISO
week over local transcripts and fails when one appears. It judges shell scripts with the collector's
own recognizers. Python uses a heuristic, a statement-starting process call with a commit or PR verb on
its line. That heuristic's first two runs both flagged scripts that only quoted such a call inside a
string, which is why the call must start the statement. Recovery for the shell case is cheap on the parser: when a shell
program's script is a literal `-c` argument or a literal heredoc, parse it recursively. That is about
20 lines and stays deterministic. The Python case is not recoverable deterministically, since recognizing
a subprocess call means reading another language.

## Status

open, guarded: `TestHiddenAuthoring_Tripwire` (`HIDDEN_AUTHORING_PROBE=1`) re-measures by week and fails if one appears. 0 through W40
