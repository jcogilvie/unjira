package jira

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// positionTimeFormat is how a watermark is stored. RFC3339Nano is correct here
// and is *not* safe in contexts that compare timestamps as strings — it strips
// trailing zeros, so ".1" sorts after ".15". Nothing compares positions
// lexicographically; they are parsed. Keep it that way.
const positionTimeFormat = time.RFC3339Nano

// jqlHashLength is how much of the JQL digest goes into the position. Twelve
// hex characters is ample to detect an edit, which is all this needs to do — it
// is not a security boundary.
const jqlHashLength = 12

// CursorResource is the cursors-table resource key for one connection's named
// query. The query name is part of the key, so renaming a query resets only
// that query's watermark.
func CursorResource(connection, query string) string {
	return connection + "/" + query
}

// EncodePosition renders a cursor position as "<jql hash>:<watermark>".
//
// effectiveJQL must be the fully scoped query — the configured JQL plus the
// auto-added project clause — because widening either invalidates the
// watermark for the same reason.
func EncodePosition(effectiveJQL string, watermark time.Time) string {
	return hashJQL(effectiveJQL) + ":" + watermark.Format(positionTimeFormat)
}

// DecodePosition returns the stored watermark, and whether it is usable for
// effectiveJQL.
//
// False means "rescan from the query's own horizon", which is always safe:
// (source, external_id) dedup makes re-emission free, so the cost of a
// needless rescan is API calls. Wrongly trusting a watermark costs data, which
// is why every unreadable form returns false rather than an error a caller
// might handle by continuing.
func DecodePosition(position, effectiveJQL string) (time.Time, bool) {
	hash, raw, found := strings.Cut(position, ":")
	if !found || hash != hashJQL(effectiveJQL) {
		return time.Time{}, false
	}

	watermark, err := time.Parse(positionTimeFormat, raw)
	if err != nil {
		return time.Time{}, false
	}

	return watermark, true
}

// hashJQL digests a query for change detection.
func hashJQL(jql string) string {
	sum := sha256.Sum256([]byte(jql))

	return hex.EncodeToString(sum[:])[:jqlHashLength]
}
