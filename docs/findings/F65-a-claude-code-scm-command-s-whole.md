# F65 — a claude_code SCM command's whole text is authoring, so a PR or commit body's citations rank as SCM

`authoringText` (`internal/collector/claudecode/scm.go:147`) searches the whole source of an authoring
command, heredoc included, and the whole input of a GitHub MCP write. A `gh pr create` body or a
multi-paragraph commit message therefore puts every key it cites on `ArtifactSCMKeys`, the tier for
the author naming the ticket. The github collector draws that line for the same PR (title keys SCM,
body keys prose, code no candidate), so the two sides of one PR now disagree. On a real 30-day store,
31 claude_code events carry more than one SCM key, at most 72. Narrative 7 shows the disagreement:
PAAS-3939, cited only in the body of two helm-charts PRs, is prose on their GitHub events and
SCM-tier on the transcript segments that opened them. The model judged it `mentioned`, so nothing
went wrong there, but a lone such key would now be promoted without a model call.

Fixing it means reading the title flag apart from the body (`--title`/`-t` against `--body`/`-b`/
`--body-file`) and a commit's subject apart from its body (the first `-m`, or the first line of `-F`),
on the shell words `simpleCommands` already parses.
