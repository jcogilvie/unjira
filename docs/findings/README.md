# Open architectural findings

Problems in the current tree that nobody has fixed yet, one file per finding. Companion to
`docs/architecture.md`, which describes what unjira *is* — this directory lists where what it is
falls short of what it should be.

**Kept apart from `architecture.md` on purpose: the two have different lifecycles.** A description is
true until the code changes; a finding is open until somebody closes it. Mixing them means the
architecture doc is never in a settled state, and it makes "is this still true?" ambiguous — a stale
description misleads, while a stale finding sends someone to fix something already fixed.

**One file per finding** so that adding, fixing or updating a finding is a filesystem operation that
touches no other finding, and two PRs never conflict over adjacent entries. There is deliberately no
index here — `ls docs/findings` is the index.

## File shape

`F<n>-<short-kebab-slug>.md`, containing:

- a title line, `# F<n> — <title>`, stating the consequence;
- the body: what is wrong, with `file:line` references, and what it costs;
- optionally a final `## Status` section: open / narrowed / deferred / policy question, the linked
  task, and what has been measured.

## How to maintain these files

- **Delete the file when the finding is fixed.** Not "resolved 2026-09-03" — delete it. `git log`
  holds the history, and a struck-through entry is just a stale finding with extra steps. If the fix
  taught a general lesson, that lesson belongs in `docs/design-notes.md` as a numbered incident.
- **Renumber nothing, and never reuse a number.** The `F<n>` labels are stable handles for
  cross-references, including from task descriptions and commit messages. A gap where F3 used to be
  is correct and informative. Because fixed findings are deleted, the highest number in this
  directory is not the highest ever used: a new finding takes
  max(numbers in this directory, numbers in `git log --all` messages and diffs — e.g.
  `git log --all -p -- docs/findings docs/architecture-findings.md | grep -o 'F[0-9]\+'` —
  and numbers in open PRs) + 1. Two PRs that pick the same number anyway show up in review as two
  files with the same `F<n>-` prefix; renumber the newer one before merge.
- **Every finding carries `file:line`** so a reader can check whether it still holds. A finding that
  cannot be checked is an opinion.
- **State the consequence, not just the observation.** "This is inconsistent" is not a finding;
  "a future GitHub collector would be silently invisible to this check" is.
- None of these are prescriptions. The linked tasks are where shape decisions belong.
