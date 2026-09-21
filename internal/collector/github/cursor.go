package github

import (
	"time"

	ghclient "github.com/jcogilvie/unjira/internal/clients/github"
)

// positionTimeFormat is how a watermark is stored — RFC3339Nano, matching
// collector/jira's own cursor codec, and for the same reason: positions are
// always parsed, never compared as strings, so its trailing-zero-stripping
// behaviour is harmless.
const positionTimeFormat = time.RFC3339Nano

// CursorResource is the cursors-table resource key for one configured repo:
// its fully host-qualified form ("github.com/owner/repo"), not the
// as-configured string — so "owner/repo" and "github.com/owner/repo" in
// config (which ParseRepoRef resolves identically) share one cursor row
// rather than two.
//
// No query-hash component, unlike collector/jira's CursorResource: there is
// no operator-authored query to invalidate here, only a fixed repos[] list.
// Adding or removing a repo from config changes which cursor ROWS exist, not
// what an existing row's watermark means.
func CursorResource(ref ghclient.RepoRef) string {
	return ref.String()
}

// EncodePosition renders a cursor position as the watermark alone.
func EncodePosition(watermark time.Time) string {
	return watermark.Format(positionTimeFormat)
}

// DecodePosition returns the stored watermark, and whether it was usable.
//
// False means "rescan from the repo's own backfill horizon," which is always
// safe: (source, external_id) dedup makes re-emission free, so the cost of a
// needless rescan is API calls. Wrongly trusting a corrupted local cursor row
// would cost missed PRs, which is why an unreadable form returns false rather
// than an error a caller might otherwise be tempted to handle by continuing
// with a zero-value watermark (which would silently mean "collect
// everything," the opposite of what an unreadable cursor should produce).
func DecodePosition(position string) (time.Time, bool) {
	watermark, err := time.Parse(positionTimeFormat, position)
	if err != nil {
		return time.Time{}, false
	}

	return watermark, true
}
