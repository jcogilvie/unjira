# F67 — a session that reads a corpus of other work is narrated as work, linked by every key it read

A Claude Code session whose job is to read other sessions (a journal scan, a transcript audit)
repeats in its own text the keys of everything it read. The collector cannot tell that from work:
both are a session in a directory, with prose that names tickets. On a real 30-day store, every such
session ran in one directory, a journal repository: 76 events, of which 14 were clustered into 6
narratives ("meta-claude self-tracking automated scans", "claude-files jsonl journal scan", ...: 2,
30, 53, 66, 88, 107). They hold 48 issue links between them (5 to 10 each), and drew 4 proposed
comments and 2 suppressed drafts. 42 of the 76 events open with the same scripted instruction to run
one scan command; one long session in that directory carries 72 SCM-tier keys, from commits to the
journal itself.

Excluding the directory (`collectors.claude_code.exclude_cwds`) removes all of it, and is the operator's
call: it is a configuration fact about which directories hold work. What no configuration can say is
which session in a work directory is reading rather than working, so the class recurs wherever such a
session runs elsewhere. Matching's prompt does say a candidate may be only cited, and F64 is why a
model that doubted these links still produced comments. Unmeasured beyond the one directory.
