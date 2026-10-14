# F51 — a dry run reports an extend's title and window from the cluster result, not as Persist writes them

`describeUnpersisted` (`internal/pipeline/narrate.go:413`) builds each dry-run narrative from the
cluster result. For an extend that adds members, it reports `r.Title` (line 430) and a window of
`eventWindow(r.Events)` (line 421), meaning the earliest and latest of this pass's new members. Persist
reports something else for the same extend (`internal/correlator/correlator.go:1565`, `:1586`). It keeps
the stored title, because the prompt asks for a title only on `new` (F27), so `r.Title` is empty. It keeps
the stored `window_start`, and moves `window_end` only forward. Checked with a throwaway test: a
narrative `Old` over `[09:00, 14:00]` extended by one event at 12:30 reports `title="Old"
window=[09:00, 14:00]` from a real pass and `title="" window=[12:30, 12:30]` from a dry run.
**Consequence:** `NarrateOptions.DryRun` promises the reported narratives are exactly what would have
been persisted, but every extend-with-members row in a dry run's output has a blank title and a window
that can be narrower than the narrative's. Only the report is affected. Clustering, placements and the
prompt are the same as the real pass's. The fix is to report an extend from the existing narrative's
row, the way the context-only branch already does. Not fixed here because it is a separate defect from
the clustering context.

## Status

open. Report only: clustering and placements match the real pass. Found fixing F46
