# F49 — transcript handling has only been tested against macOS-written transcripts

Every transcript the collector has been measured or tested against was written on macOS: 0 of 46,343
commands contain a carriage return, and every `cwd` is a POSIX path. Nothing has exercised transcripts
written on Windows or Linux. The parts that touch a platform's text conventions are the shell parser
(CRLF), `jsonlLines` (line framing), `projectName` and `exclude_cwds` matching (path separators, drive
letters), and the summary flattening in `truncate`. **Action item:** add fixture transcripts written on
those platforms and run the collector tests against them. No defect is known.

## Status

open, action item: fixture transcripts from Windows and Linux
