package jira

import "time"

// WatermarkClauseForTest exposes watermarkClause to the external test package.
//
// The clause is unexported because nothing outside this package should build a
// JQL bound, but it needs direct tests: the account-timezone rule it encodes is
// invisible to the collector's HTTP-level tests (the fake does not interpret
// JQL) and expensive to probe through the live tier.
func WatermarkClauseForTest(watermark time.Time, accountZone, connName string) string {
	return watermarkClause(watermark, accountZone, connName)
}

// WatermarkZoneFallbackMarginForTest exposes the fallback widening so tests
// assert against the real constant rather than a copy that could drift.
const WatermarkZoneFallbackMarginForTest = watermarkZoneFallbackMargin
